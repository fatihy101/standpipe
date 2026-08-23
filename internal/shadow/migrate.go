package shadow

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

// migration is one forward-only schema step. There is no rollback: you restore
// the snapshot instead.
type migration struct {
	Version  int
	Name     string
	Source   string
	Checksum string
}

// migrations is shadow.db's OWN list, and its independence from
// internal/store's is the whole of the second reason for a separate file. A
// Phase 1 schema change adds a file here and changes nothing of Phase 0's.
var migrations = loadMigrations()

func loadMigrations() []migration {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		panic("shadow: reading embedded migrations: " + err.Error())
	}
	out := make([]migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			panic("shadow: migration file name must be <version>_<name>.sql: " + e.Name())
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			panic("shadow: migration file name must start with an integer version: " + e.Name())
		}
		src, err := fs.ReadFile(migrationFS, path.Join("migrations", e.Name()))
		if err != nil {
			panic("shadow: reading embedded migration " + e.Name() + ": " + err.Error())
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
			panic(fmt.Sprintf("shadow: migration versions must be contiguous from 1, found %d at position %d", m.Version, i+1))
		}
	}
	return out
}

// LatestSchemaVersion is the version this build expects shadow.db to be at.
func LatestSchemaVersion() int { return len(migrations) }

// SchemaVersion reads PRAGMA user_version. shadow.db carries its own,
// independent of gateway.db's.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	if err := db.reader.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("shadow: reading user_version: %w", err)
	}
	return v, nil
}

// MigrationStep describes one pending migration.
type MigrationStep struct {
	Version  int
	Name     string
	Checksum string
}

// MigrationPlan lists the migrations that would be applied, in order.
func (db *DB) MigrationPlan(ctx context.Context) ([]MigrationStep, error) {
	have, err := db.SchemaVersion(ctx)
	if err != nil {
		return nil, err
	}
	if have > LatestSchemaVersion() {
		return nil, fmt.Errorf("%w: shadow.db is at %d, this build knows %d", ErrSchemaNewer, have, LatestSchemaVersion())
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

// Migrate applies every pending migration, each in its own transaction. It is
// idempotent.
//
// Before applying anything it verifies the checksums of the migrations the
// database already carries. Without that pass the guard is unreachable on a
// current database: the plan skips every applied version, so applyMigration —
// verifyChecksum's only caller — never runs for one, and an edited migration
// file is accepted in silence by exactly the code written to catch it.
//
// Migrate does NOT snapshot. The caller owns that; call BackupTo first.
func (db *DB) Migrate(ctx context.Context) ([]MigrationStep, error) {
	plan, err := db.MigrationPlan(ctx)
	if err != nil {
		return nil, err
	}
	if err := db.VerifyAppliedChecksums(ctx); err != nil {
		return nil, err
	}
	applied := make([]MigrationStep, 0, len(plan))
	for _, step := range plan {
		m := migrations[step.Version-1]
		if err := db.applyMigration(ctx, m); err != nil {
			return applied, fmt.Errorf("shadow: migration %d (%s): %w", m.Version, m.Name, err)
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
			return verifyChecksumTx(ctx, tx, m)
		}
		if have != m.Version-1 {
			return fmt.Errorf("out of order: shadow.db is at %d", have)
		}
		if _, err := tx.ExecContext(ctx, m.Source); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_version (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, m.Name, m.Checksum, db.cfg.now().UnixMilli()); err != nil {
			return err
		}
		// PRAGMA takes no bound parameters; the value is an int parsed from an
		// embedded file name, not user input.
		if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.Version)); err != nil {
			return err
		}
		return nil
	})
}

// VerifyAppliedChecksums re-reads schema_version for every migration shadow.db
// has already applied and compares it with the embedded source. It writes
// nothing and repairs nothing.
//
// Unlike internal/store's version there is deliberately NO backfill path: that
// exists there for databases predating the schema_version table, and shadow.db
// has no such history. A missing row here means user_version was edited by
// hand, and inventing the row would hide it.
func (db *DB) VerifyAppliedChecksums(ctx context.Context) error {
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
			return fmt.Errorf("%w: user_version says %d is applied but schema_version has no row for it", ErrSchemaChecksum, m.Version)
		case err != nil:
			return err
		case stored != m.Checksum:
			return fmt.Errorf("%w: migration %d (%s): shadow.db recorded %s, this build embeds %s — the migration file was edited after it was applied",
				ErrSchemaChecksum, m.Version, m.Name, stored, m.Checksum)
		}
	}
	return nil
}

func verifyChecksumTx(ctx context.Context, tx *sql.Conn, m migration) error {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT checksum FROM schema_version WHERE version = ?`, m.Version).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: user_version says %d is applied but schema_version has no row for it", ErrSchemaChecksum, m.Version)
	case err != nil:
		return err
	case stored != m.Checksum:
		return fmt.Errorf("%w: shadow.db has %s, build has %s", ErrSchemaChecksum, stored, m.Checksum)
	}
	return nil
}

// MigrateFile opens shadow.db, snapshots it, migrates it and closes it.
// snapshotDir may be empty, in which case the snapshot lands beside the file.
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
	// A dry run must surface a tampered migration too, and must apply nothing.
	if err := db.VerifyAppliedChecksums(ctx); err != nil {
		return nil, "", err
	}
	if dryRun || len(plan) == 0 {
		return plan, "", nil
	}

	// Snapshot before migrating. Only meaningful once there is content.
	if have, verr := db.SchemaVersion(ctx); verr == nil && have > 0 {
		dir := snapshotDir
		if dir == "" {
			dir = filepath.Dir(db.Path())
		}
		snapshot = filepath.Join(dir, "shadow-backup-"+db.cfg.now().UTC().Format("20060102T150405Z")+".db")
		if err := db.BackupTo(ctx, snapshot); err != nil {
			return nil, "", fmt.Errorf("shadow: pre-migration snapshot: %w", err)
		}
	}

	steps, err = db.Migrate(ctx)
	return steps, snapshot, err
}
