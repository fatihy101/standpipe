package shadow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE OPEN-SIDE VERSION GATE, WHICH NOTHING WAS HOLDING EITHER.
//
// internal/store has TestOpenRefusesOutdatedSchema and TestOpenRefusesNewerSchema.
// internal/shadow had neither, and the two errors ErrSchemaOutdated and
// ErrSchemaNewer were asserted by nothing in the repository. Measured the same
// way the checksum gates were measured last round — by deleting the guard and
// running the whole repo, not by reading it:
//
//	sqlite.go Open, `case have < want` deleted        -> go test ./... GREEN
//	sqlite.go Open, `case have > want` deleted        -> go test ./... GREEN
//	sqlite.go Open, both branches say ErrSchemaOutdated -> GREEN
//	sqlite.go Open, db.Close() dropped on refusal     -> GREEN
//	sqlite.go Open, `return db, err` instead of nil   -> GREEN
//	migrate.go MigrationPlan, ErrSchemaNewer guard deleted -> GREEN
//	migrate.go VerifyAppliedChecksums, loop stopped after version 1 -> GREEN
//
// The last one is the one internal/store cannot reach at all: store has ONE
// migration, so its tamper tests can only ever touch version 1 and a loop that
// checks only its first element is indistinguishable there from a correct one.
// shadow has four, so the loop can be exercised past its first iteration, and
// TestOpenChecksEveryAppliedMigrationNotJustTheFirst does exactly that.
//
// Note what is NOT here. `want := LatestSchemaVersion()` needs no test of its
// own: mutating it to +1 or to 1 is already killed by the two checksum tests in
// migrate_checksum_test.go, which call Open on a current ledger and would get a
// version error instead of ErrSchemaChecksum. That is coverage arriving from
// somewhere it was not designed to, and it is worth more than a test that
// restates it.

// shadowPathAtVersion builds a shadow.db that stopped partway, by applying the
// first `upto` migrations through the real applyMigration path. It leaves no
// handle open on the file.
//
// This is the shape internal/store's migration tests cannot construct: with one
// migration there is no "partway". Every intermediate version here is a genuine
// database — user_version and schema_version agree, the checksums are right,
// and the only thing wrong with it is that it is not finished.
func shadowPathAtVersion(t *testing.T, upto int) (string, Config) {
	t.Helper()
	if upto < 0 || upto > LatestSchemaVersion() {
		t.Fatalf("shadowPathAtVersion(%d): this build has %d migrations", upto, LatestSchemaVersion())
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shadow.db")
	cfg := Config{Path: path}
	db, err := OpenUnchecked(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	for _, m := range migrations {
		if m.Version > upto {
			break
		}
		if err := db.applyMigration(ctx, m); err != nil {
			db.Close()
			t.Fatalf("applying migration %d (%s): %v", m.Version, m.Name, err)
		}
	}
	have, err := db.SchemaVersion(ctx)
	if err != nil {
		db.Close()
		t.Fatalf("SchemaVersion: %v", err)
	}
	if have != upto {
		db.Close()
		t.Fatalf("built a ledger at version %d, want %d", have, upto)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the build handle: %v", err)
	}
	return path, cfg
}

// shadowUserVersion reads PRAGMA user_version through a plain handle, so the
// assertion does not depend on the package it is checking.
func shadowUserVersion(t *testing.T, path string) int {
	t.Helper()
	h := rawShadowSQL(t, path)
	var v int
	if err := h.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
	return v
}

// TestOpenRefusesOutdatedSchema is the gateway's startup rule for the ledger: a pending migration
// means the daemon does not start, rather than writing drill evidence into a
// schema it does not have.
func TestOpenRefusesOutdatedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow.db")
	cfg := Config{Path: path}

	db, err := Open(context.Background(), cfg)
	if db != nil {
		db.Close()
		t.Fatalf("Open returned a usable ledger alongside its refusal; a caller that checks the handle rather than the error gets a database the build does not understand")
	}
	if !errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("Open on a version-0 shadow.db = %v, want ErrSchemaOutdated", err)
	}
	// The two refusals must stay distinguishable: `standpipe migrate` is the
	// answer to one of them and there is no answer to the other, so a caller
	// that cannot tell them apart is told to run a migration that will not help.
	if errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("the outdated refusal also satisfies errors.Is(ErrSchemaNewer); the two are one error and the operator cannot act on them differently")
	}
}

// TestOpenRefusesAPartiallyMigratedLedger is the same gate at every version
// between the first and the last, which is the interval internal/store's single
// migration does not have. It is not hypothetical: Migrate applies each step in
// its own transaction, so an interrupted `standpipe migrate` leaves exactly one
// of these on disk.
func TestOpenRefusesAPartiallyMigratedLedger(t *testing.T) {
	if LatestSchemaVersion() < 2 {
		t.Fatalf("this test exists because shadow has more than one migration; it now has %d, and the interval it covers has disappeared", LatestSchemaVersion())
	}
	for v := 1; v < LatestSchemaVersion(); v++ {
		t.Run(migrations[v-1].Name, func(t *testing.T) {
			_, cfg := shadowPathAtVersion(t, v)

			db, err := Open(context.Background(), cfg)
			if db != nil {
				db.Close()
				t.Fatalf("Open returned a ledger stopped at version %d of %d", v, LatestSchemaVersion())
			}
			if !errors.Is(err, ErrSchemaOutdated) {
				t.Fatalf("Open on a ledger at version %d of %d = %v, want ErrSchemaOutdated",
					v, LatestSchemaVersion(), err)
			}
			// Not a checksum failure: every row this ledger carries is correct.
			// Distinguishing them matters because the repair differs — one is
			// `standpipe migrate`, the other is a human reading a diff.
			if errors.Is(err, ErrSchemaChecksum) {
				t.Fatalf("an unfinished ledger was reported as a checksum divergence: %v", err)
			}

			// And it is unfinished in the way the plan says it is.
			udb, err := OpenUnchecked(context.Background(), cfg)
			if err != nil {
				t.Fatalf("OpenUnchecked: %v", err)
			}
			defer udb.Close()
			plan, err := udb.MigrationPlan(context.Background())
			if err != nil {
				t.Fatalf("MigrationPlan: %v", err)
			}
			if len(plan) != LatestSchemaVersion()-v {
				t.Fatalf("plan has %d steps at version %d, want %d", len(plan), v, LatestSchemaVersion()-v)
			}
			if plan[0].Version != v+1 {
				t.Fatalf("plan resumes at version %d, want %d", plan[0].Version, v+1)
			}
		})
	}
}

// TestOpenRefusesNewerSchema is the no-downgrade rule. It uses
// LatestSchemaVersion()+1 rather than a distant number on purpose: the nearest
// newer version is the boundary, and a guard that is off by one is only visible
// there.
func TestOpenRefusesNewerSchema(t *testing.T) {
	path, cfg := migratedShadowPath(t)
	future := LatestSchemaVersion() + 1

	h := rawShadowSQL(t, path)
	if _, err := h.Exec("PRAGMA user_version = " + strconv.Itoa(future)); err != nil {
		t.Fatalf("bumping user_version: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}

	db, err := Open(context.Background(), cfg)
	if db != nil {
		db.Close()
		t.Fatalf("Open returned a ledger written by a newer build")
	}
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Open on a version-%d shadow.db = %v, want ErrSchemaNewer", future, err)
	}
	if errors.Is(err, ErrSchemaOutdated) {
		t.Fatalf("the newer refusal also satisfies errors.Is(ErrSchemaOutdated); the operator is being told to run a migration that does not exist")
	}

	// The migrate path refuses too, and refuses BEFORE touching anything. A
	// newer ledger has rows this build cannot read; migrating it forward from a
	// version this build does not know is how those rows are lost.
	udb, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	defer udb.Close()
	if _, err := udb.MigrationPlan(context.Background()); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("MigrationPlan on a version-%d shadow.db = %v, want ErrSchemaNewer", future, err)
	}
	if _, err := udb.Migrate(context.Background()); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Migrate on a version-%d shadow.db = %v, want ErrSchemaNewer", future, err)
	}
	if got := shadowUserVersion(t, path); got != future {
		t.Fatalf("after the refusals user_version is %d, want it untouched at %d", got, future)
	}
}

// TestOpenAcceptsTheBuildsOwnVersion is the positive control, and it was
// missing: no test in the repository called shadow.Open on a healthy ledger.
// Without it every refusal above is satisfied by an Open that refuses always.
func TestOpenAcceptsTheBuildsOwnVersion(t *testing.T) {
	_, cfg := migratedShadowPath(t)

	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open on a fully migrated shadow.db = %v, want success", err)
	}
	defer db.Close()
	if err := db.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck on the ledger Open just accepted: %v", err)
	}
	if v, err := db.SchemaVersion(context.Background()); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("the accepted ledger is at version %d (err %v), want %d", v, err, LatestSchemaVersion())
	}
}

// TestOpenChecksEveryAppliedMigrationNotJustTheFirst is the one internal/store
// structurally cannot write. store has a single migration, so a checksum loop
// that examines only its first element passes every test store can express.
// shadow has four. Each is tampered with in turn, on its own file.
func TestOpenChecksEveryAppliedMigrationNotJustTheFirst(t *testing.T) {
	if LatestSchemaVersion() < 2 {
		t.Fatalf("this test exists to exercise the checksum loop past its first iteration; shadow now has %d migrations and cannot", LatestSchemaVersion())
	}
	for _, m := range migrations {
		t.Run(m.Name, func(t *testing.T) {
			path, cfg := migratedShadowPath(t)

			h := rawShadowSQL(t, path)
			res, err := h.Exec(`UPDATE schema_version SET checksum = 'deadbeef' WHERE version = ?`, m.Version)
			if err != nil {
				t.Fatalf("tampering with schema_version: %v", err)
			}
			if n, _ := res.RowsAffected(); n != 1 {
				t.Fatalf("tampered %d rows for version %d, want 1", n, m.Version)
			}
			if err := h.Close(); err != nil {
				t.Fatalf("closing raw handle: %v", err)
			}

			db, err := Open(context.Background(), cfg)
			if db != nil {
				db.Close()
				t.Fatalf("Open accepted a ledger whose migration %d (%s) no longer matches the embedded source", m.Version, m.Name)
			}
			if !errors.Is(err, ErrSchemaChecksum) {
				t.Fatalf("Open with migration %d tampered = %v, want ErrSchemaChecksum", m.Version, err)
			}
			// The refusal has to name the file, or the operator has four
			// migrations and no way to tell which one drifted.
			if !strings.Contains(err.Error(), m.Name) {
				t.Fatalf("the refusal does not name migration %d (%s): %v", m.Version, m.Name, err)
			}
		})
	}
}

// TestMigrateVerifiesTheAppliedPrefixOfAPartialLedger pins both sides of the
// checksum loop's boundary on a ledger that is midway through: what it must
// check, and what it must NOT.
//
// It must check every version at or below user_version — a resumed migration is
// exactly the moment a drifted earlier file gets built upon. It must not check
// the versions above it, because those have no schema_version row yet and are
// not supposed to; treating their absence as a divergence would make every
// unfinished migration unresumable.
func TestMigrateVerifiesTheAppliedPrefixOfAPartialLedger(t *testing.T) {
	if LatestSchemaVersion() < 2 {
		t.Fatalf("this test needs a ledger that can be partway through; shadow has %d migrations", LatestSchemaVersion())
	}
	stop := LatestSchemaVersion() - 1

	t.Run("resumes", func(t *testing.T) {
		path, cfg := shadowPathAtVersion(t, stop)
		db, err := OpenUnchecked(context.Background(), cfg)
		if err != nil {
			t.Fatalf("OpenUnchecked: %v", err)
		}
		defer db.Close()
		applied, err := db.Migrate(context.Background())
		if err != nil {
			t.Fatalf("Migrate on a ledger at version %d = %v; the pending migrations have no schema_version row and must not be read as missing ones", stop, err)
		}
		if len(applied) != LatestSchemaVersion()-stop {
			t.Fatalf("Migrate applied %d migrations from version %d, want %d", len(applied), stop, LatestSchemaVersion()-stop)
		}
		if got := shadowUserVersion(t, path); got != LatestSchemaVersion() {
			t.Fatalf("after resuming, user_version is %d, want %d", got, LatestSchemaVersion())
		}
	})

	t.Run("refuses a drifted earlier migration", func(t *testing.T) {
		path, cfg := shadowPathAtVersion(t, stop)

		h := rawShadowSQL(t, path)
		res, err := h.Exec(`UPDATE schema_version SET checksum = 'deadbeef' WHERE version = ?`, stop)
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
		if _, err := db.Migrate(context.Background()); !errors.Is(err, ErrSchemaChecksum) {
			t.Fatalf("Migrate on a partial ledger whose version %d drifted = %v, want ErrSchemaChecksum", stop, err)
		}
		if got := shadowUserVersion(t, path); got != stop {
			t.Fatalf("the refused Migrate advanced user_version to %d; it must apply nothing, since the pending migrations would be built on a schema nobody can describe", got)
		}
	})
}

// TestARefusedOpenClosesWhatItOpened.
//
// Open builds both pools before it can read user_version, so every refusal is
// reached with two live sql.DBs in hand. Dropping the db.Close() from either
// refusal branch leaves them open, and the daemon that retries on ErrSchemaOutdated
// in a supervision loop leaks a writer connection per attempt until it runs out
// of descriptors — with the whole suite still green.
//
// It is observed through SQLite's own bookkeeping rather than through fds:
// the -wal and -shm sidecars are deleted when the LAST connection to a WAL
// database closes, so their survival after a refused Open is the leaked pool,
// stated by the file system. Measured: with db.Close() removed from the newer
// branch, both files remain.
func TestARefusedOpenClosesWhatItOpened(t *testing.T) {
	path, cfg := migratedShadowPath(t)

	h := rawShadowSQL(t, path)
	if _, err := h.Exec("PRAGMA user_version = " + strconv.Itoa(LatestSchemaVersion()+1)); err != nil {
		t.Fatalf("bumping user_version: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("%s exists before the refused Open, so its survival afterwards would prove nothing", path+suffix)
		}
	}

	db, err := Open(context.Background(), cfg)
	if db != nil {
		db.Close()
		t.Fatalf("Open returned a handle alongside its refusal")
	}
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Open = %v, want ErrSchemaNewer", err)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("%s survives a refused Open: the connection pools were left open, and a supervisor that retries the refusal leaks one writer per attempt", path+suffix)
		}
	}
}
