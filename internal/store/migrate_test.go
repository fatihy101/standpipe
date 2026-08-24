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

	// errors.Is, not merely non-nil: the caller of `standpipe migrate` has to be
	// able to tell a checksum divergence from an out-of-order version or an I/O
	// failure, and a bare `err != nil` passes for all three.
	if _, err := db.Migrate(context.Background()); !errors.Is(err, ErrSchemaChecksum) {
		t.Fatalf("Migrate on a tampered checksum = %v, want ErrSchemaChecksum; "+
			"a divergence between what the database contains and what the build thinks it contains went unreported", err)
	}
}

// TestOpenRefusesTamperedChecksum is the STARTUP half of the same gate, and it
// is a different code path from the one above, not a restatement of it.
// TestMigrateRefusesTamperedChecksum exercises Migrate — the operator command,
// run deliberately, allowed to repair. This exercises Open — the daemon's own
// entry, which must repair nothing and must refuse to serve.
//
// It replaces TestOpenIgnoresChecksumAtStartup, which fbb768d wrote to
// DOCUMENT that Open had no checksum gate and which 23efda3 then closed. From
// that commit until this one the test skipped itself on every run — a tripwire
// that had already fired and was never picked up. The behaviour it asserted no
// longer exists on any reachable path, so the assertion is inverted here rather
// than deleted: deleting it would have left sqlite.go's VerifyAppliedChecksums
// call with no coverage in the repository at all. Measured — with that call
// removed, `go test ./...` was green.
func TestOpenRefusesTamperedChecksum(t *testing.T) {
	path, cfg := migratedPath(t)

	h := rawSQL(t, path)
	if _, err := h.Exec(`UPDATE schema_version SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatalf("tampering with schema_version: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	db, err := Open(context.Background(), cfg)
	if err == nil {
		db.Close()
		t.Fatalf("Open accepted a database whose recorded checksum does not match the embedded migration; " +
			"the gateway would serve requests against a schema it does not know it has")
	}
	if !errors.Is(err, ErrSchemaChecksum) {
		t.Fatalf("Open on a tampered checksum = %v, want ErrSchemaChecksum", err)
	}

	// And it refused without rewriting what it refused over. A startup gate that
	// "corrects" the row it disagreed with destroys the only evidence of the
	// divergence, and the next start is silently clean.
	h2 := rawSQL(t, path)
	var stored string
	if err := h2.QueryRow(`SELECT checksum FROM schema_version WHERE version = 1`).Scan(&stored); err != nil {
		t.Fatalf("re-reading schema_version: %v", err)
	}
	if stored != "deadbeef" {
		t.Fatalf("after a refused Open the recorded checksum is %q, want it untouched at \"deadbeef\"; "+
			"the startup gate repaired the evidence it refused over", stored)
	}
}

// TestOpenRefusesAMissingChecksumRowAndMigrateBackfillsIt pins the ONE place
// the two gates deliberately differ, which is the whole reason
// verifyAppliedChecksums takes a backfill flag.
//
// user_version says version 1 is applied and schema_version has no row for it:
// a hand-edited user_version, or a file from a build predating the table. The
// operator command may record what it found; the daemon may not, because
// writing the row makes the next start look clean without anyone having
// checked what the database actually contains.
//
// Asserted in both directions on purpose. Testing only the refusal would still
// pass if backfill were removed from Migrate as well, and testing only the
// backfill would still pass if Open started backfilling too.
func TestOpenRefusesAMissingChecksumRowAndMigrateBackfillsIt(t *testing.T) {
	path, cfg := migratedPath(t)

	h := rawSQL(t, path)
	res, err := h.Exec(`DELETE FROM schema_version WHERE version = 1`)
	if err != nil {
		t.Fatalf("deleting the schema_version row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("deleted %d rows, want 1", n)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	db, err := Open(context.Background(), cfg)
	if err == nil {
		db.Close()
		t.Fatalf("Open accepted a database whose user_version claims a migration schema_version has no record of")
	}
	if !errors.Is(err, ErrSchemaChecksum) {
		t.Fatalf("Open on a missing schema_version row = %v, want ErrSchemaChecksum", err)
	}

	h2 := rawSQL(t, path)
	var n int
	if err := h2.QueryRow(`SELECT count(*) FROM schema_version WHERE version = 1`).Scan(&n); err != nil {
		t.Fatalf("counting schema_version rows: %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
	if n != 0 {
		t.Fatalf("the refused Open backfilled the missing schema_version row; " +
			"a read-only startup check must not repair anything it cannot explain")
	}

	// The other direction: `standpipe migrate` is the path that may record it.
	mdb, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	if _, err := mdb.Migrate(context.Background()); err != nil {
		mdb.Close()
		t.Fatalf("Migrate over a missing schema_version row: %v", err)
	}
	if err := mdb.Close(); err != nil {
		t.Fatalf("closing the migrate handle: %v", err)
	}

	h3 := rawSQL(t, path)
	var stored string
	if err := h3.QueryRow(`SELECT checksum FROM schema_version WHERE version = 1`).Scan(&stored); err != nil {
		t.Fatalf("Migrate did not backfill the missing schema_version row: %v", err)
	}
	if err := h3.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
	if stored != migrations[0].Checksum {
		t.Fatalf("Migrate backfilled checksum %q, want the embedded %q", stored, migrations[0].Checksum)
	}

	// And the backfill is enough to make the daemon's gate pass again — which is
	// what makes the refusal above a REFUSAL rather than a permanent brick.
	reopened, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open after the migrate-path backfill: %v", err)
	}
	reopened.Close()
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
