package store

import (
	"testing"
	"time"
)

// seedFullTriggerChain writes one trigger with the complete set of children
// the retention purge has to delete before it: a trigger_outcome, a run_link and a
// run_observed. It returns the trigger's event id.
//
// The children are deliberately given a RECENT ts even when the parent is old.
// The purge's first statement selects children by their PARENT's ts, not their own; a
// child predicate written on the child's own ts would leave these behind and
// then blow up on the second statement's foreign key.
func (e *testEnv) seedFullTriggerChain(t *testing.T, suffix string, parentTS time.Time, rid int64) string {
	t.Helper()
	c := ctx(t)
	eventID := "evt_chain_" + suffix

	e.clock.Set(parentTS)
	req := reserveReq("acme", "key-chain-"+suffix, "POST /t/acme/core/release.yml", bodyHash(suffix), eventID)
	req.Event.TS = parentTS
	if _, err := e.Reserve(c, req); err != nil {
		t.Fatalf("Reserve(%s): %v", suffix, err)
	}
	// The children are written "now" — long after the parent in the old case.
	e.clock.Set(testBase())
	comp := completeReq("acme", "key-chain-"+suffix, eventID, StateComplete, 200, "{}")
	comp.Outcome.EventID = "evt_chain_out_" + suffix
	comp.RunLink = &RunLinkEvent{EventID: "evt_chain_link_" + suffix, RunID: runID(rid)}
	if _, err := e.Complete(c, comp); err != nil {
		t.Fatalf("Complete(%s): %v", suffix, err)
	}
	if _, err := e.AppendRunObserved(c, RunObservedEvent{
		EventID: "evt_chain_obs_" + suffix, TriggerEventID: eventID,
		RunID: rid, Conclusion: "success",
	}); err != nil {
		t.Fatalf("AppendRunObserved(%s): %v", suffix, err)
	}
	return eventID
}

// TestPurgeOldEventsWithChildrenAttached covers the trigger log retention purge.
//
// Two claims, and the second is the one that has already been wrong once:
//
//  1. Ordering. trigger_event_id is a real foreign key and the connection runs
//     with PRAGMA foreign_keys = ON, so deleting parents before children — which is
//     what an earlier design said — is a constraint violation, not a slow path. The
//     test therefore proves the foreign key is actually armed first; without
//     that proof "no FK error" is a statement about nothing.
//  2. Completeness. The purge's first three statements enumerate event types
//     LITERALLY, so trigger_rejected — parentless, and the highest-volume type
//     in the table — is skipped by all three. Statement 4 exists for it. If it
//     is missing, retention silently stops applying to most of the log and
//     nothing anywhere fails.
func TestPurgeOldEventsWithChildrenAttached(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	// Claim 1's precondition: the foreign key is enforced on the write pool.
	// AppendRunLink names a parent that does not exist; with the pragma off
	// this succeeds and the whole ordering argument below is vacuous.
	if _, err := env.AppendRunLink(c, RunLinkEvent{
		EventID: "evt_dangling", TriggerEventID: "evt_does_not_exist",
		RunID: runID(1), Resolution: ResolutionExact, ResolutionSource: SourceRunName,
	}); err == nil {
		t.Fatalf("a run_link naming a nonexistent parent was accepted — PRAGMA foreign_keys is OFF and this test proves nothing")
	}

	old := testBase().Add(-100 * 24 * time.Hour)
	recent := testBase().Add(-1 * time.Hour)

	env.seedFullTriggerChain(t, "old", old, 100)
	env.seedFullTriggerChain(t, "new", recent, 200)

	// Parentless trigger_rejected rows, one either side of the cutoff.
	for _, r := range []struct {
		id string
		ts time.Time
	}{{"evt_rej_old", old}, {"evt_rej_new", recent}} {
		ev := sampleRejected(r.id)
		ev.TS = r.ts
		if _, err := env.AppendTriggerRejected(c, ev); err != nil {
			t.Fatalf("AppendTriggerRejected(%s): %v", r.id, err)
		}
	}

	env.clock.Set(testBase())
	if _, err := env.PurgeOldEvents(c, 0); err == nil {
		t.Errorf("PurgeOldEvents accepted a zero retention")
	}

	before := env.countRows(t, `SELECT COUNT(*) FROM trigger_events`)
	if before != 10 {
		t.Fatalf("%d events before the purge, want 10 (2 chains of 4 + 2 rejections)", before)
	}

	n, err := env.PurgeOldEvents(c, 90*24*time.Hour)
	if err != nil {
		// This is the ordering failure: FOREIGN KEY constraint failed, because the
		// parent went first.
		t.Fatalf("PurgeOldEvents with children attached: %v", err)
	}
	// The old chain (4 rows) plus the old rejection (1 row).
	if n != 5 {
		t.Errorf("PurgeOldEvents = %d rows, want 5 (trigger + outcome + run_link + run_observed + trigger_rejected)", n)
	}

	survived := map[string]bool{}
	rows, err := env.reader.QueryContext(c, `SELECT event_id FROM trigger_events`)
	if err != nil {
		t.Fatalf("listing survivors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		survived[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	wantGone := []string{
		"evt_chain_old", "evt_chain_out_old", "evt_chain_link_old", "evt_chain_obs_old",
		"evt_rej_old",
	}
	for _, id := range wantGone {
		if survived[id] {
			t.Errorf("%s survived the purge", id)
		}
	}
	wantKept := []string{
		"evt_chain_new", "evt_chain_out_new", "evt_chain_link_new", "evt_chain_obs_new",
		"evt_rej_new",
	}
	for _, id := range wantKept {
		if !survived[id] {
			t.Errorf("%s was deleted; only rows older than the retention may go", id)
		}
	}
	if len(survived) != 5 {
		t.Errorf("%d events survived, want 5", len(survived))
	}
	// Idempotent: a second run at the same clock has nothing left to delete.
	if n, err := env.PurgeOldEvents(c, 90*24*time.Hour); err != nil || n != 0 {
		t.Errorf("second PurgeOldEvents = %d rows, err %v; want 0", n, err)
	}

	// The idempotency table is NOT touched — the TTL purge owns that, on its own TTL.
	if got := env.countRows(t, `SELECT COUNT(*) FROM idempotency`); got != 2 {
		t.Errorf("%d idempotency rows after the event purge, want 2 — event retention must not touch them", got)
	}
}

// TestPurgeOldEventsKeepsAChildWhoseParentSurvives is the narrow case the
// purge's first statement could get wrong in the other direction: a child that is itself
// older than the cutoff but hangs off a parent that is not.
//
// The child must SURVIVE. Deleting it would silently drop the outcome of a
// retained trigger, and the background resolver reads exactly that outcome to decide
// whether a trigger was ever forwarded — the trigger would re-enter the
// resolver queue forever.
func TestPurgeOldEventsKeepsAChildWhoseParentSurvives(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	recent := testBase().Add(-time.Hour)
	env.clock.Set(recent)
	req := reserveReq("acme", "key-young-parent", "POST /t/acme/core/release.yml", bodyHash("yp"), "evt_yp")
	req.Event.TS = recent
	if _, err := env.Reserve(c, req); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// A child stamped long before its parent. Impossible in production, and
	// that is the point: the predicate must key on the parent, so even this
	// child stays.
	comp := completeReq("acme", "key-young-parent", "evt_yp", StateComplete, 200, "{}")
	comp.Outcome.EventID = "evt_yp_out"
	comp.Outcome.TS = testBase().Add(-100 * 24 * time.Hour)
	if _, err := env.Complete(c, comp); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	env.clock.Set(testBase())
	n, err := env.PurgeOldEvents(c, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldEvents: %v", err)
	}
	if n != 0 {
		t.Errorf("PurgeOldEvents = %d rows, want 0 — the parent is inside the retention window", n)
	}
	if got := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_id = ?`, "evt_yp_out"); got != 1 {
		t.Errorf("the outcome of a retained trigger was deleted on its own ts; the resolver's forwarded-EXISTS would then never match again")
	}
}
