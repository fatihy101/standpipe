package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one forward-only schema step (no rollback; you restore
// the snapshot instead).
type migration struct {
	Version  int
	Name     string
	Source   string
	Checksum string
}

var migrations = loadMigrations()

func loadMigrations() []migration {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		panic("store: reading embedded migrations: " + err.Error())
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			panic("store: migration file name must be <version>_<name>.sql: " + e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			panic("store: migration file name must start with an integer version: " + e.Name())
		}
		src, err := fs.ReadFile(migrationFS, path.Join("migrations", e.Name()))
		if err != nil {
			panic("store: reading embedded migration " + e.Name() + ": " + err.Error())
		}
		sum := sha256.Sum256(src)
		out = append(out, migration{
			Version:  v,
			Name:     strings.TrimSuffix(e.Name(), ".sql"),
			Source:   string(src),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			panic(fmt.Sprintf("store: migration versions must be contiguous from 1, found %d at position %d", m.Version, i+1))
		}
	}
	return out
}

// LatestSchemaVersion is the version this build expects the database to be at.
func LatestSchemaVersion() int { return len(migrations) }

// SchemaVersion reads PRAGMA user_version, the cheap startup check.
// The schema_version table carries the detail (name, checksum, applied_at);
// user_version is kept in step with it inside the same transaction so the two
// can never disagree.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := db.reader.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("store: reading user_version: %w", err)
	}
	return v, nil
}

// MigrationStep describes one pending migration for `standpipe migrate --dry-run`.
type MigrationStep struct {
	Version  int
	Name     string
	Checksum string
}

// MigrationPlan lists the migrations that would be applied, in order. An empty
// slice means the database is current.
func (db *DB) MigrationPlan(ctx context.Context) ([]MigrationStep, error) {
	have, err := db.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	if have > LatestSchemaVersion() {
		return nil, fmt.Errorf("%w: database is at %d, this build knows %d", ErrSchemaNewer, have, LatestSchemaVersion())
	}
	var out []MigrationStep
	for _, m := range migrations {
		if m.Version <= have {
			continue
		}
		out = append(out, MigrationStep{Version: m.Version, Name: m.Name, Checksum: m.Checksum})
	}
	return out, nil
}

// Migrate applies every pending migration, each in its own transaction.
// It is idempotent: a migration already recorded in schema_version is
// skipped, so re-running Migrate on a current database is a no-op.
//
// Before applying anything it verifies the checksums of the migrations the
// database already carries. Without that pass the guard was unreachable on a
// current database: the plan skips every version at or below user_version, so
// applyMigration — verifyChecksum's only caller — never ran for an
// already-applied migration, and an edited migration file was accepted in
// silence by exactly the code written to catch it.
//
// Migrate does NOT take the required pre-migration VACUUM INTO snapshot —
// the caller owns that, because the snapshot path is a
// configuration concern. Call BackupTo first.
func (db *DB) Migrate(ctx context.Context) ([]MigrationStep, error) {
	plan, err := db.MigrationPlan(ctx)
	if err != nil {
		return nil, err
	}
	if err := db.verifyAppliedChecksums(ctx, true); err != nil {
		return nil, err
	}
	applied := make([]MigrationStep, 0, len(plan))
	for _, step := range plan {
		m := migrations[step.Version-1]
		if err := db.applyMigration(ctx, m); err != nil {
			return applied, fmt.Errorf("store: migration %d (%s): %w", m.Version, m.Name, err)
		}
		applied = append(applied, step)
	}
	return applied, nil
}

func (db *DB) applyMigration(ctx context.Context, m migration) error {
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		// Re-check inside the transaction: another process may have applied it
		// between MigrationPlan and here.
		var have int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&have); err != nil {
			return err
		}
		if have >= m.Version {
			return db.verifyChecksum(ctx, tx, m)
		}
		if have != m.Version-1 {
			return fmt.Errorf("out of order: database is at %d", have)
		}
		if _, err := tx.ExecContext(ctx, m.Source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, m.Name, m.Checksum, db.cfg.now().UnixMilli()); err != nil {
			return err
		}
		// PRAGMA takes no bound parameters; the value is an int from an
		// embedded file name, not user input.
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.Version)); err != nil {
			return err
		}
		return nil
	})
}

// VerifyAppliedChecksums re-reads schema_version for every migration this
// database has already applied and compares it with the embedded source.
// It writes nothing.
//
// This is the read-only gate. Open calls it at startup, so a database whose
// content diverges from the build's idea of it never serves a request.
func (db *DB) VerifyAppliedChecksums(ctx context.Context) error {
	return db.verifyAppliedChecksums(ctx, false)
}

// verifyAppliedChecksums checks versions 1..user_version. With backfill it may
// insert a missing schema_version row (the `standpipe migrate` path); without it
// a missing row is reported, because a read-only startup check must not repair
// anything it cannot explain.
func (db *DB) verifyAppliedChecksums(ctx context.Context, backfill bool) error {
	have, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if m.Version > have {
			break
		}
		var stored string
		err := db.reader.QueryRowContext(ctx,
			`SELECT checksum FROM schema_version WHERE version = ?`, m.Version).Scan(&stored)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if !backfill {
				return fmt.Errorf("%w: user_version says %d is applied but schema_version has no row for it", ErrSchemaChecksum, m.Version)
			}
			// user_version says applied but the table has no row: a
			// hand-edited user_version, or a database from a build that
			// predates this table. Record it rather than guess.
			if err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO schema_version (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
					m.Version, m.Name, m.Checksum, db.cfg.now().UnixMilli())
				return err
			}); err != nil {
				return err
			}
		case err != nil:
			return err
		case stored != m.Checksum:
			return fmt.Errorf("%w: migration %d (%s): database recorded %s, this build embeds %s — the migration file was edited after it was applied",
				ErrSchemaChecksum, m.Version, m.Name, stored, m.Checksum)
		}
	}
	return nil
}

// verifyChecksum guards against a migration file being edited after it was
// applied somewhere. A silent divergence between what a database contains and
// what the build thinks it contains is exactly the failure this table exists
// to catch.
//
// This is the in-transaction variant, used when another process applied the
// migration between MigrationPlan and applyMigration.
func (db *DB) verifyChecksum(ctx context.Context, tx *sql.Conn, m migration) error {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT checksum FROM schema_version WHERE version = ?`, m.Version).Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		// user_version says applied but the table has no row: a database
		// migrated by a build that predates the schema_version table, or a
		// hand-edited user_version. Record it rather than guess.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, m.Name, m.Checksum, db.cfg.now().UnixMilli())
		return err
	case err != nil:
		return err
	case stored != m.Checksum:
		return fmt.Errorf("%w: database has %s, build has %s", ErrSchemaChecksum, stored, m.Checksum)
	}
	return nil
}

// MigrateFile opens path, snapshots it, migrates it and closes it. It is the
// whole of `standpipe migrate` minus the flag parsing. snapshotDir may be empty,
// in which case the snapshot lands beside the database.
func MigrateFile(ctx context.Context, cfg Config, snapshotDir string, dryRun bool) (steps []MigrationStep, snapshot string, err error) {
	db, err := OpenUnchecked(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	defer db.Close()

	plan, err := db.MigrationPlan(ctx)
	if err != nil {
		return nil, "", err
	}
	if dryRun {
		// A dry run must surface a tampered migration too, and it must not
		// repair anything: --dry-run applies nothing.
		if err := db.VerifyAppliedChecksums(ctx); err != nil {
			return nil, "", err
		}
		return plan, "", nil
	}
	if len(plan) == 0 {
		// Still verify: a no-op migrate on a current database is exactly when
		// an edited migration file has to be reported.
		if err := db.verifyAppliedChecksums(ctx, true); err != nil {
			return nil, "", err
		}
		return plan, "", nil
	}

	// Snapshot before migrating. Only meaningful once the database has
	// content — a version 0 database has nothing to lose.
	if have, verr := db.SchemaVersion(ctx); verr == nil && have > 0 {
		dir := snapshotDir
		if dir == "" {
			dir = filepath.Dir(db.Path())
		}
		snapshot = filepath.Join(dir, "gateway-backup-"+db.cfg.now().UTC().Format("20060102T150405Z")+".db")
		if err := db.BackupTo(ctx, snapshot); err != nil {
			return nil, "", fmt.Errorf("store: pre-migration snapshot: %w", err)
		}
	}

	steps, err = db.Migrate(ctx)
	return steps, snapshot, err
}
