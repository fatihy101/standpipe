package store

import (
	"errors"
	"testing"
	"time"
)

// sweepParams are the crash-recovery sweep knobs at their default config shape: a queue ceiling
// in seconds and an upstream ceiling an order of magnitude larger. The gap
// between them is exactly where an earlier sweep bug lived.
var sweepParams = SweepParams{
	UpstreamTimeout: 30 * time.Second,
	MaxQueueWait:    5 * time.Second,
	Margin:          10 * time.Second,
}

// TestSweepBoundaryDispatchedRowSurvives is the sweep boundary regression.
//
// A row that HAS a dispatch_started_at must be measured against
// upstream_timeout + margin, never against max_queue_wait + margin. The window
// under test is the gap between the two thresholds: at 20s elapsed the queue
// threshold (15s) has passed but the dispatch threshold (40s) has not. If the
// sweep flips this row, a retry can reclaim the key while the original call is
// still travelling to GitHub — a double dispatch.
func TestSweepBoundaryDispatchedRowSurvives(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	path := "POST /t/acme/core/release.yml"

	res, err := env.Reserve(c, reserveReq("acme", "key-b2", path, body, "evt_b2"))
	if err != nil || res.Outcome != ReserveNew {
		t.Fatalf("Reserve = %v, %v; want new", res.Outcome, err)
	}
	if err := env.MarkDispatchStarted(c, "acme", "key-b2", "evt_b2"); err != nil {
		t.Fatalf("MarkDispatchStarted: %v", err)
	}

	// Every instant strictly inside (max_queue_wait+margin, upstream_timeout+margin).
	queueThreshold := sweepParams.MaxQueueWait + sweepParams.Margin       // 15s
	dispatchThreshold := sweepParams.UpstreamTimeout + sweepParams.Margin // 40s
	for _, elapsed := range []time.Duration{
		queueThreshold + time.Millisecond,
		20 * time.Second,
		dispatchThreshold - time.Millisecond,
		dispatchThreshold, // the cutoff is strict `<`, so equality must survive
	} {
		env.clock.Set(testBase().Add(elapsed))
		n, err := env.SweepStaleInFlight(c, sweepParams)
		if err != nil {
			t.Fatalf("SweepStaleInFlight at %s: %v", elapsed, err)
		}
		if n != 0 {
			t.Fatalf("swept %d rows at %s after dispatch start; want 0 — sweep boundary regression, this is a double dispatch", n, elapsed)
		}
		rec, err := env.LookupIdempotency(c, "acme", "key-b2")
		if err != nil {
			t.Fatalf("LookupIdempotency: %v", err)
		}
		if rec.State != StateInFlight {
			t.Fatalf("state = %q at %s, want in_flight", rec.State, elapsed)
		}
	}

	// Past the dispatch threshold it must be swept, otherwise a crashed
	// process leaks the key until the TTL expires.
	env.clock.Set(testBase().Add(dispatchThreshold + time.Millisecond))
	n, err := env.SweepStaleInFlight(c, sweepParams)
	if err != nil {
		t.Fatalf("SweepStaleInFlight: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows past upstream_timeout+margin, want 1", n)
	}
	rec, err := env.LookupIdempotency(c, "acme", "key-b2")
	if err != nil {
		t.Fatalf("LookupIdempotency: %v", err)
	}
	if rec.State != StateUnknown {
		t.Errorf("state = %q, want unknown", rec.State)
	}
}

// TestSweepQueuedRowUsesQueueThreshold pins the other half of the sweep's
// condition, and with it the ordering contract on MarkDispatchStarted: a row
// with NO dispatch_started_at is measured against max_queue_wait + margin.
//
// This is the failure mode the store's own doc comment warns about. A caller
// that dispatches first and stamps afterwards leaves the row looking queued
// for the whole duration of the GitHub call, and at 16s — well inside a 30s
// upstream timeout — the sweeper takes it. The assertion below is what makes
// "MarkDispatchStarted must have RETURNED before the GitHub request is issued"
// a checked contract rather than a comment.
func TestSweepQueuedRowUsesQueueThreshold(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	body := bodyHash(`{"inputs":{}}`)

	if _, err := env.Reserve(c, reserveReq("acme", "key-queued", "POST /t/acme/core/release.yml", body, "evt_q")); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// No MarkDispatchStarted: the request is still queued as far as the row
	// can tell.

	env.clock.Set(testBase().Add(sweepParams.MaxQueueWait + sweepParams.Margin))
	if n, err := env.SweepStaleInFlight(c, sweepParams); err != nil || n != 0 {
		t.Fatalf("swept %d rows (err %v) exactly at the queue threshold; want 0 (strict <)", n, err)
	}

	env.clock.Set(testBase().Add(sweepParams.MaxQueueWait + sweepParams.Margin + time.Millisecond))
	n, err := env.SweepStaleInFlight(c, sweepParams)
	if err != nil {
		t.Fatalf("SweepStaleInFlight: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows past the queue threshold, want 1", n)
	}
}

// TestSweepRejectsNonPositiveParams: a zero threshold would sweep every
// in-flight row on the first tick.
func TestSweepRejectsNonPositiveParams(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	for _, p := range []SweepParams{
		{UpstreamTimeout: 0, MaxQueueWait: time.Second},
		{UpstreamTimeout: time.Second, MaxQueueWait: 0},
	} {
		if _, err := env.SweepStaleInFlight(c, p); err == nil {
			t.Errorf("SweepStaleInFlight(%+v) = nil error, want a refusal", p)
		}
	}
}

// TestCrashRecoveryRoundTrip is crash recovery plus the replay rule, end to end.
//
//	Reserve -> MarkDispatchStarted -> process dies (no Complete)
//	restart  -> sweep flips the row to unknown
//	retry    -> Reserve returns reclaimed with a NEW event_id
//	the dead attempt's Complete must NOT be applied
func TestCrashRecoveryRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	path := "POST /t/acme/core/release.yml"

	first, err := env.Reserve(c, reserveReq("acme", "key-crash", path, body, "evt_crash_1"))
	if err != nil || first.Outcome != ReserveNew {
		t.Fatalf("Reserve = %v, %v; want new", first.Outcome, err)
	}
	if err := env.MarkDispatchStarted(c, "acme", "key-crash", "evt_crash_1"); err != nil {
		t.Fatalf("MarkDispatchStarted: %v", err)
	}
	dispatchedAt := env.clock.Now()

	// Hard kill: no Complete, no graceful shutdown. Reopen the file.
	env.reopen(t)

	env.clock.Set(testBase().Add(sweepParams.UpstreamTimeout + sweepParams.Margin + time.Second))
	if n, err := env.SweepStaleInFlight(c, sweepParams); err != nil || n != 1 {
		t.Fatalf("sweep after restart = %d rows, err %v; want 1", n, err)
	}

	second, err := env.Reserve(c, reserveReq("acme", "key-crash", path, body, "evt_crash_2"))
	if err != nil {
		t.Fatalf("retry Reserve: %v", err)
	}
	if second.Outcome != ReserveReclaimed {
		t.Fatalf("retry outcome = %q, want reclaimed", second.Outcome)
	}
	if !second.Outcome.MayDispatch() {
		t.Errorf("reclaimed must permit a dispatch after the replay resolver check")
	}
	if second.EventID == first.EventID {
		t.Errorf("retry reused event_id %q; the reservation CAS writes a NEW event id", second.EventID)
	}
	if second.EventID != "evt_crash_2" {
		t.Errorf("retry event_id = %q, want evt_crash_2", second.EventID)
	}
	if second.PriorState != StateUnknown {
		t.Errorf("PriorState = %q, want unknown", second.PriorState)
	}
	// The replay rule needs the ORIGINAL window to run the resolver against.
	if !second.OriginalAttemptAt.Equal(testBase()) {
		t.Errorf("OriginalAttemptAt = %v, want %v", second.OriginalAttemptAt, testBase())
	}
	if second.OriginalDispatchAt == nil {
		t.Fatalf("OriginalDispatchAt is nil; the dead attempt did reach the dispatch stage")
	}
	if !second.OriginalDispatchAt.Equal(dispatchedAt) {
		t.Errorf("OriginalDispatchAt = %v, want %v", *second.OriginalDispatchAt, dispatchedAt)
	}

	// The row now points at the retry, and dispatch_started_at was cleared.
	rec, err := env.LookupIdempotency(c, "acme", "key-crash")
	if err != nil {
		t.Fatalf("LookupIdempotency: %v", err)
	}
	if rec.EventID != "evt_crash_2" || rec.State != StateInFlight {
		t.Errorf("row = %q/%q, want evt_crash_2/in_flight", rec.EventID, rec.State)
	}
	if rec.DispatchStartedAt != nil {
		t.Errorf("dispatch_started_at = %v, want NULL after the CAS", *rec.DispatchStartedAt)
	}

	// The zombie's response arrives late. It must not overwrite the retry.
	zombie, err := env.Complete(c, completeReq("acme", "key-crash", "evt_crash_1", StateComplete, 200, `{"ok":true}`))
	if err != nil {
		t.Fatalf("zombie Complete: %v", err)
	}
	if zombie.Applied {
		t.Fatalf("the dead attempt's Complete was APPLIED; it overwrote a live reservation")
	}
	// ... but the dispatch really happened, so its outcome is still logged.
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'trigger_outcome' AND trigger_event_id = ?`, "evt_crash_1"); n != 1 {
		t.Errorf("got %d trigger_outcome rows for the zombie attempt, want 1 — a real GitHub call would be unrecorded", n)
	}
	rec, err = env.LookupIdempotency(c, "acme", "key-crash")
	if err != nil {
		t.Fatalf("LookupIdempotency: %v", err)
	}
	if rec.State != StateInFlight || rec.EventID != "evt_crash_2" {
		t.Errorf("row = %q/%q after the zombie completed, want in_flight/evt_crash_2", rec.State, rec.EventID)
	}

	// The zombie also must not be able to stamp dispatch on the retry's row.
	err = env.MarkDispatchStarted(c, "acme", "key-crash", "evt_crash_1")
	if !errors.Is(err, ErrNotOwner) {
		t.Errorf("zombie MarkDispatchStarted = %v, want ErrNotOwner", err)
	}
}
