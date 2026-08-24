package shadow

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// THE STARTUP CHECKSUM GATE, WHICH NOTHING WAS HOLDING.
//
// sqlite.go's Open calls VerifyAppliedChecksums so that a shadow.db whose
// content diverges from the build's idea of it is never opened. That call had
// no coverage anywhere in the repository: it was deleted and `go test ./...`
// stayed green. This file is the store-side repair of the same shape, applied
// here — the two databases carry the same guard and it was equally unheld.
//
// The gates are NOT identical across the two packages, and the difference is
// asserted rather than assumed. internal/store backfills a missing
// schema_version row on the `standpipe migrate` path, because gateway.db has
// history predating that table. shadow.db has no such history, so a missing row
// is a hand-edited user_version on EVERY path and is refused on every path.
// Testing only the tampered-checksum case would leave that difference resting
// on a comment.

// migratedShadowPath returns a migrated shadow.db and the Config that reaches
// it, with no handle left open on the file.
func migratedShadowPath(t *testing.T) (string, Config) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shadow.db")
	cfg := Config{Path: path}
	db, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	if _, err := db.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatalf("Migrate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the migration handle: %v", err)
	}
	return path, cfg
}

// rawShadowSQL opens a plain handle on the file so a test can corrupt what the
// ledger believes about itself.
func rawShadowSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	h, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func TestOpenRefusesTamperedChecksum(t *testing.T) {
	path, cfg := migratedShadowPath(t)

	h := rawShadowSQL(t, path)
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

	db, err := Open(context.Background(), cfg)
	if err == nil {
		db.Close()
		t.Fatalf("Open accepted a shadow.db whose recorded checksum does not match the embedded migration; " +
			"the drill would record its evidence into a schema it does not know it has")
	}
	if !errors.Is(err, ErrSchemaChecksum) {
		t.Fatalf("Open on a tampered checksum = %v, want ErrSchemaChecksum", err)
	}

	// Refused without rewriting what it refused over. A gate that "corrects" the
	// row it disagreed with destroys the only evidence of the divergence.
	h2 := rawShadowSQL(t, path)
	var stored string
	if err := h2.QueryRow(`SELECT checksum FROM schema_version WHERE version = 1`).Scan(&stored); err != nil {
		t.Fatalf("re-reading schema_version: %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
	if stored != "deadbeef" {
		t.Fatalf("after a refused Open the recorded checksum is %q, want it untouched at \"deadbeef\"", stored)
	}
}

// TestNoPathBackfillsAMissingChecksumRow is the difference from internal/store,
// stated as an assertion. shadow.db predates nothing: every path refuses.
func TestNoPathBackfillsAMissingChecksumRow(t *testing.T) {
	path, cfg := migratedShadowPath(t)

	h := rawShadowSQL(t, path)
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

	if db, err := Open(context.Background(), cfg); !errors.Is(err, ErrSchemaChecksum) {
		if err == nil {
			db.Close()
		}
		t.Fatalf("Open on a missing schema_version row = %v, want ErrSchemaChecksum", err)
	}

	mdb, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	_, merr := mdb.Migrate(context.Background())
	if err := mdb.Close(); err != nil {
		t.Fatalf("closing the migrate handle: %v", err)
	}
	if !errors.Is(merr, ErrSchemaChecksum) {
		t.Fatalf("Migrate on a missing schema_version row = %v, want ErrSchemaChecksum; "+
			"shadow.db has no pre-schema_version history, so inventing the row would hide a hand-edited user_version", merr)
	}

	h2 := rawShadowSQL(t, path)
	var n int
	if err := h2.QueryRow(`SELECT count(*) FROM schema_version WHERE version = 1`).Scan(&n); err != nil {
		t.Fatalf("counting schema_version rows: %v", err)
	}
	if err := h2.Close(); err != nil {
		t.Fatalf("closing raw handle: %v", err)
	}
	if n != 0 {
		t.Fatalf("a path backfilled the missing schema_version row; shadow's gates repair nothing")
	}
}
