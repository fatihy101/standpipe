package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/fatihy101/standpipe/internal/testutil"
)

// rawSQL opens a second, plain handle on the same file so a test can corrupt
// what the store believes about itself. Only the migration tests use it.
func rawSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func migratedPath(t *testing.T) (string, Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	clock := testutil.NewClock(testutil.BaseTime)
	cfg := Config{Path: path, Now: clock.Now}
	migrateFresh(t, cfg)
	return path, cfg
}

// TestMigrateAppliesAndIsIdempotent is the baseline: a fresh file reaches the
// build's version, and a second Migrate is a no-op.
func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	cfg := Config{Path: path}

	db, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	defer db.Close()

	if v, err := db.SchemaVersion(context.Background()); err != nil || v != 0 {
		t.Fatalf("fresh database is at version %d (err %v), want 0", v, err)
	}
	plan, err := db.MigrationPlan(context.Background())
	if err != nil {
		t.Fatalf("MigrationPlan: %v", err)
	}
	if len(plan) != LatestSchemaVersion() {
		t.Fatalf("plan has %d steps, want %d", len(plan), LatestSchemaVersion())
	}

	applied, err := db.Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != LatestSchemaVersion() {
		t.Fatalf("applied %d migrations, want %d", len(applied), LatestSchemaVersion())
	}
	if v, err := db.SchemaVersion(context.Background()); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("after Migrate the database is at %d (err %v), want %d", v, err, LatestSchemaVersion())
	}

	again, err := db.Migrate(context.Background())
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second Migrate applied %d migrations, want 0", len(again))
	}
}

// TestOpenRefusesOutdatedSchema: the gateway must refuse to start
// against a database a migration is pending on, rather than run against a
// schema it does not understand.
func TestOpenRefusesOutdatedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	cfg := Config{Path: path}
	if _, err := Open(context.Background(), cfg); !errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("Open on a version-0 database = %v, want ErrSchemaOutdated", err)
	}
}

// TestOpenRefusesNewerSchema is the no-downgrade rule: a database
// written by a newer build is not opened at all.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path, cfg := migratedPath(t)

	h := rawSQL(t, path)
	if _, err := h.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("bumping user_version: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	if _, err := Open(context.Background(), cfg); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Open on a future-version database = %v, want ErrSchemaNewer", err)
	}

	db, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	defer db.Close()
	if _, err := db.MigrationPlan(context.Background()); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("MigrationPlan on a future-version database = %v, want ErrSchemaNewer", err)
	}
	if _, err := db.Migrate(context.Background()); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Migrate on a future-version database = %v, want ErrSchemaNewer", err)
	}
}

// TestMigrateRefusesTamperedChecksum.
//
// schema_version.checksum exists to catch a migration file that was edited
// after it was applied: the database then contains something other than what
// the build believes it contains. verifyChecksum's own doc comment calls that
// "exactly the failure this table exists to catch".
//
// The tamper simulated here is the realistic one — the checksum recorded in
// the database no longer matches the embedded migration source, on a database
// that is otherwise at the current version. Migrate must refuse.
func TestMigrateRefusesTamperedChecksum(t *testing.T) {
	path, cfg := migratedPath(t)

	h := rawSQL(t, path)
	res, err := h.Exec(`UPDATE schema_version SET checksum = 'deadbeef' WHERE version = 1`)
	if err != nil {
		t.Fatalf("tampering with schema_version: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("tampered %d rows, want 1", n)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	db, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	defer db.Close()

	if _, err := db.Migrate(context.Background()); err == nil {
		t.Fatalf("Migrate accepted a database whose recorded checksum does not match the embedded migration; " +
			"a divergence between what the database contains and what the build thinks it contains went unreported")
	}
}

// TestOpenAcceptsTamperedChecksum documents the same hole from the startup
// side: if Migrate is the only checksum gate, Open never sees it either.
// Recorded as an assertion so the day the gate moves, this test says so.
func TestOpenIgnoresChecksumAtStartup(t *testing.T) {
	path, cfg := migratedPath(t)

	h := rawSQL(t, path)
	if _, err := h.Exec(`UPDATE schema_version SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatalf("tampering: %v", err)
	}
	h.Close()

	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Skipf("Open now validates checksums (%v); TestMigrateRefusesTamperedChecksum is the live gate", err)
	}
	db.Close()
	t.Log("Open() does not verify schema_version.checksum; the only gate is Migrate (see TestMigrateRefusesTamperedChecksum)")
}

// TestMigrationsAreContiguousAndStable: the embedded set is well formed, and
// a checksum is a real sha256 of the source rather than a placeholder.
func TestMigrationsAreContiguousAndStable(t *testing.T) {
	if LatestSchemaVersion() == 0 {
		t.Fatalf("no migrations are embedded")
	}
	seen := map[int]bool{}
	for i, m := range migrations {
		if m.Version != i+1 {
			t.Fatalf("migration %d has version %d, want %d", i, m.Version, i+1)
		}
		if seen[m.Version] {
			t.Fatalf("duplicate migration version %d", m.Version)
		}
		seen[m.Version] = true
		if len(m.Checksum) != 64 {
			t.Errorf("migration %d checksum %q is not a 64-char hex sha256", m.Version, m.Checksum)
		}
		if m.Source == "" {
			t.Errorf("migration %d has an empty source", m.Version)
		}
	}
}

// TestMigrateFileTakesSnapshot is the pre-migration VACUUM INTO, and its
// --dry-run counterpart.
func TestMigrateFileDryRunAndSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.db")
	cfg := Config{Path: path, Now: testutil.NewClock(testutil.BaseTime).Now}

	steps, snapshot, err := MigrateFile(context.Background(), cfg, "", true)
	if err != nil {
		t.Fatalf("MigrateFile dry run: %v", err)
	}
	if len(steps) != LatestSchemaVersion() {
		t.Errorf("dry run listed %d steps, want %d", len(steps), LatestSchemaVersion())
	}
	if snapshot != "" {
		t.Errorf("dry run produced snapshot %q, want none", snapshot)
	}

	// A version-0 database has nothing to lose, so no snapshot is taken.
	steps, snapshot, err = MigrateFile(context.Background(), cfg, "", false)
	if err != nil {
		t.Fatalf("MigrateFile: %v", err)
	}
	if len(steps) != LatestSchemaVersion() {
		t.Errorf("applied %d steps, want %d", len(steps), LatestSchemaVersion())
	}
	if snapshot != "" {
		t.Errorf("snapshot %q taken for a version-0 database, want none", snapshot)
	}

	// Now that it is populated, a further MigrateFile is a no-op with no
	// snapshot (the plan is empty).
	steps, _, err = MigrateFile(context.Background(), cfg, "", false)
	if err != nil {
		t.Fatalf("second MigrateFile: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("second MigrateFile applied %d steps, want 0", len(steps))
	}
}

// TestBackupToAndHealthCheck covers backup and the health check on a live database.
func TestBackupToAndHealthCheck(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	if _, err := env.Reserve(c, reserveReq("acme", "k", "POST /p", bodyHash("b"), "evt_bk")); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := env.HealthCheck(c); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	snapshot := filepath.Join(t.TempDir(), "snap.db")
	if err := env.BackupTo(c, snapshot); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	// A VACUUM INTO snapshot is self-contained: it opens and reads without a
	// -wal file beside it.
	restored, err := Open(context.Background(), Config{Path: snapshot})
	if err != nil {
		t.Fatalf("opening the snapshot: %v", err)
	}
	defer restored.Close()
	rec, err := restored.LookupIdempotency(context.Background(), "acme", "k")
	if err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if rec.EventID != "evt_bk" {
		t.Errorf("snapshot holds event_id %q, want evt_bk", rec.EventID)
	}
	if err := restored.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck on the snapshot: %v", err)
	}

	// VACUUM INTO refuses to overwrite.
	if err := env.BackupTo(c, snapshot); err == nil {
		t.Errorf("BackupTo overwrote an existing snapshot")
	}
}

// TestOpenRejectsEmptyPath — a Config with no path must not silently create a
// database somewhere.
func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := OpenUnchecked(context.Background(), Config{Path: "  "}); err == nil {
		t.Fatalf("OpenUnchecked accepted an empty path")
	}
}
