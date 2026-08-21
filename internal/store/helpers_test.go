package store

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/testutil"
)

// These tests live in `package store` rather than `package store_test` on
// purpose: content_sha256 stability has to be checked against the hash
// scope itself, not only against what the export happens to print, and the
// NULL-vs-empty-string distinctions have to be read as raw SQL.
// internal/testutil therefore imports nothing from this package — a store ->
// testutil -> store cycle would not build.

// testEnv is one migrated database plus the clock and id source driving it.
type testEnv struct {
	*DB
	clock *testutil.Clock
	ids   *testutil.IDSeq
	path  string
	cfg   Config
}

// newTestEnv creates a fresh database, migrates it, and opens it through the
// checked Open path — the same sequence an operator performs (`standpipe
// migrate`, then start the gateway).
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvAt(t, filepath.Join(t.TempDir(), "gateway.db"))
}

func newTestEnvAt(t *testing.T, path string) *testEnv {
	t.Helper()
	clock := testutil.NewClock(testutil.BaseTime)
	ids := testutil.NewIDSeq("evt_gen_")
	cfg := Config{
		Path:       path,
		Now:        clock.Now,
		NewEventID: ids.Next,
	}
	migrateFresh(t, cfg)
	env := &testEnv{clock: clock, ids: ids, path: path, cfg: cfg}
	env.DB = openChecked(t, cfg)
	return env
}

func migrateFresh(t *testing.T, cfg Config) {
	t.Helper()
	raw, err := OpenUnchecked(context.Background(), cfg)
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	if _, err := raw.Migrate(context.Background()); err != nil {
		raw.Close()
		t.Fatalf("Migrate: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("closing migration handle: %v", err)
	}
}

func openChecked(t *testing.T, cfg Config) *DB {
	t.Helper()
	db, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// reopen closes the handle and opens the same file again, standing in for a
// process restart after a hard kill.
func (e *testEnv) reopen(t *testing.T) {
	t.Helper()
	if err := e.DB.Close(); err != nil {
		t.Fatalf("closing before reopen: %v", err)
	}
	e.DB = openChecked(t, e.cfg)
}

// testBase is the instant every test timeline starts at.
func testBase() time.Time { return testutil.BaseTime }

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return c
}

// bodyHash is the 32-byte digest the reservation protocol keys on. It is not
// BodySHA256 so that a test can produce a mismatching digest without having to
// invent a second valid JSON body.
func bodyHash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// reserveReq builds a ReserveRequest with the mandatory fields filled in.
func reserveReq(caller, key, path string, body []byte, eventID string) ReserveRequest {
	return ReserveRequest{
		CallerName:     caller,
		CallerTokenID:  "tok_" + caller + "_1",
		IdempotencyKey: key,
		RequestPath:    path,
		BodySHA256:     body,
		TTL:            24 * time.Hour,
		Event: TriggerEvent{
			EventID:          eventID,
			TargetProject:    "acme/core",
			TargetWorkflow:   "release.yml",
			TargetRef:        "refs/heads/main",
			InputsHash:       "hmac-sha256:9f3c1a2b7d4e5f60:deadbeef",
			InputsValidation: ValidationDefaultBranch,
			// Correlation is required and has no default. The
			// fixture arms layer 2, which is what a default deployment does;
			// tests that care about a different layer set it themselves.
			Correlation:     CorrelationInjected,
			DecisionBackend: "github",
			DecisionReason:  "observation_mode",
			RequestID:       "req_" + eventID,
		},
	}
}

// completeReq builds a CompleteRequest for an owned reservation.
func completeReq(caller, key, eventID string, final State, status int, body string) CompleteRequest {
	outcomeStatus := OutcomeForwarded
	if final == StateUnknown {
		outcomeStatus = OutcomeError
	}
	return CompleteRequest{
		CallerName:     caller,
		IdempotencyKey: key,
		EventID:        eventID,
		FinalState:     final,
		ResponseStatus: status,
		ResponseBody:   body,
		Outcome: TriggerOutcomeEvent{
			OutcomeStatus:     outcomeStatus,
			OutcomeHTTPStatus: status,
			UpstreamDuration:  342 * time.Millisecond,
		},
	}
}

// rawScan runs a read-only query against the store's own read pool. Tests use
// it only where the difference between SQL NULL and a zero value is the thing
// under test.
func (e *testEnv) rawScan(t *testing.T, dest []any, query string, args ...any) {
	t.Helper()
	if err := e.DB.reader.QueryRowContext(ctx(t), query, args...).Scan(dest...); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
}

func (e *testEnv) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	e.rawScan(t, []any{&n}, query, args...)
	return n
}
