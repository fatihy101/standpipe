package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestReserveTriggerInsertFailureRollsBack is the reserve step's atomicity claim.
//
// The reservation INSERT and the trigger event INSERT share one transaction.
// If the event write fails — here because its event_id already exists — the
// reservation must not survive. A reservation whose event_id points at nothing
// is a dispatch with no record, which is worse than no dispatch at all.
func TestReserveTriggerInsertFailureRollsBack(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	// Occupy the event id with an unrelated, already-logged event. A
	// pre-reservation rejection is a parentless trigger_rejected row, not a
	// trigger + trigger_outcome pair.
	if _, err := env.AppendTriggerRejected(c, TriggerRejectedEvent{
		EventID:        "evt_taken",
		CallerName:     "acme",
		CallerTokenID:  "tok_acme_1",
		TargetProject:  "acme/core",
		TargetWorkflow: "release.yml",
		RejectReason:   RejectUnsupportedInputs,
		HTTPStatus:     422,
	}); err != nil {
		t.Fatalf("AppendTriggerRejected: %v", err)
	}

	req := reserveReq("acme", "key-atomic", "POST /t/acme/core/release.yml",
		bodyHash(`{"inputs":{}}`), "evt_taken")
	res, err := env.Reserve(c, req)
	if err == nil {
		t.Fatalf("Reserve succeeded with a duplicate event_id (outcome %q); want an error", res.Outcome)
	}
	if !IsConstraint(err) {
		t.Errorf("Reserve error = %v; want a SQLite constraint violation", err)
	}

	// The whole point: no idempotency row.
	if _, err := env.LookupIdempotency(c, "acme", "key-atomic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an idempotency row survived a failed trigger insert (LookupIdempotency err = %v)", err)
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM idempotency`); n != 0 {
		t.Fatalf("idempotency table holds %d rows after the rollback, want 0", n)
	}
	// And the key is still free: a corrected retry must be able to take it.
	retry, err := env.Reserve(c, reserveReq("acme", "key-atomic", "POST /t/acme/core/release.yml",
		bodyHash(`{"inputs":{}}`), "evt_fresh"))
	if err != nil {
		t.Fatalf("retry Reserve after rollback: %v", err)
	}
	if retry.Outcome != ReserveNew {
		t.Errorf("retry outcome = %q, want new — the rolled-back attempt poisoned the key", retry.Outcome)
	}
}

// TestReserveRejectsIncompleteRequests: every guard in Reserve's preamble.
// These are 500s in production, and a missing one lets a malformed
// reservation reach the table.
func TestReserveRejectsIncompleteRequests(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	good := reserveReq("acme", "k", "POST /p", bodyHash("b"), "evt_x")

	cases := map[string]func(r *ReserveRequest){
		"no caller_name":     func(r *ReserveRequest) { r.CallerName = "" },
		"no idempotency_key": func(r *ReserveRequest) { r.IdempotencyKey = "" },
		"no request_path":    func(r *ReserveRequest) { r.RequestPath = "" },
		"short body hash":    func(r *ReserveRequest) { r.BodySHA256 = []byte{1, 2, 3} },
		"nil body hash":      func(r *ReserveRequest) { r.BodySHA256 = nil },
		"zero TTL":           func(r *ReserveRequest) { r.TTL = 0 },
		"negative TTL":       func(r *ReserveRequest) { r.TTL = -time.Hour },
		"no event id":        func(r *ReserveRequest) { r.Event.EventID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := good
			mutate(&req)
			if _, err := env.Reserve(c, req); err == nil {
				t.Fatalf("Reserve accepted a request with %s", name)
			}
		})
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM idempotency`); n != 0 {
		t.Errorf("%d rows written by rejected requests, want 0", n)
	}
}

// decisionCase is one cell of the reservation protocol's decision table.
type decisionCase struct {
	name       string
	seed       State
	sameBody   bool
	samePath   bool
	wantOut    ReserveOutcome
	wantReason ConflictReason
	// specSilent marks cells the decision table does not name. The expected value
	// below is the code's documented extension, not a spec quotation.
	specSilent bool
}

// TestReserveDecisionTable walks all 3 x 2 x 2 cells of the decision table.
//
// The table spells out six rows; the remaining six cells are covered
// by an "—" (don't care) in the body or path column. Where the spec is silent
// the case is marked specSilent and the expectation is the store's own
// documented reading, flagged as such.
func TestReserveDecisionTable(t *testing.T) {
	const (
		path      = "POST /t/acme/core/release.yml"
		otherPath = "POST /t/acme/core/deploy.yml"
	)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	otherBody := bodyHash(`{"inputs":{"tag":"v2"}}`)

	cases := []decisionCase{
		// --- complete: table rows 1-3 ---
		{name: "complete/body match/path match", seed: StateComplete, sameBody: true, samePath: true, wantOut: ReserveReplay},
		{name: "complete/body mismatch/path match", seed: StateComplete, sameBody: false, samePath: true, wantOut: ReserveConflict, wantReason: ConflictBody},
		{name: "complete/body match/path mismatch", seed: StateComplete, sameBody: true, samePath: false, wantOut: ReserveConflict, wantReason: ConflictPath},
		{name: "complete/body mismatch/path mismatch", seed: StateComplete, sameBody: false, samePath: false, wantOut: ReserveConflict, wantReason: ConflictBody, specSilent: true},

		// --- in_flight: table row 4, both columns "—" ---
		{name: "in_flight/body match/path match", seed: StateInFlight, sameBody: true, samePath: true, wantOut: ReserveInProgress},
		{name: "in_flight/body mismatch/path match", seed: StateInFlight, sameBody: false, samePath: true, wantOut: ReserveInProgress},
		{name: "in_flight/body match/path mismatch", seed: StateInFlight, sameBody: true, samePath: false, wantOut: ReserveInProgress},
		{name: "in_flight/body mismatch/path mismatch", seed: StateInFlight, sameBody: false, samePath: false, wantOut: ReserveInProgress},

		// --- unknown: table rows 5-6 ---
		{name: "unknown/body match/path match", seed: StateUnknown, sameBody: true, samePath: true, wantOut: ReserveReclaimed},
		{name: "unknown/body mismatch/path match", seed: StateUnknown, sameBody: false, samePath: true, wantOut: ReserveConflict, wantReason: ConflictBody},
		{name: "unknown/body match/path mismatch", seed: StateUnknown, sameBody: true, samePath: false, wantOut: ReserveConflict, wantReason: ConflictPath, specSilent: true},
		{name: "unknown/body mismatch/path mismatch", seed: StateUnknown, sameBody: false, samePath: false, wantOut: ReserveConflict, wantReason: ConflictBody, specSilent: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			c := ctx(t)

			// Seed the row in the state under test.
			seed, err := env.Reserve(c, reserveReq("acme", "k", path, body, "evt_seed"))
			if err != nil || seed.Outcome != ReserveNew {
				t.Fatalf("seed Reserve = %v, %v; want new", seed.Outcome, err)
			}
			switch tc.seed {
			case StateInFlight:
				// already there
			case StateComplete:
				if _, err := env.Complete(c, completeReq("acme", "k", "evt_seed", StateComplete, 200, `{"dispatched":true}`)); err != nil {
					t.Fatalf("seed Complete: %v", err)
				}
			case StateUnknown:
				if _, err := env.Complete(c, completeReq("acme", "k", "evt_seed", StateUnknown, 0, "")); err != nil {
					t.Fatalf("seed Complete: %v", err)
				}
			}

			useBody, usePath := body, path
			if !tc.sameBody {
				useBody = otherBody
			}
			if !tc.samePath {
				usePath = otherPath
			}
			got, err := env.Reserve(c, reserveReq("acme", "k", usePath, useBody, "evt_second"))
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			note := ""
			if tc.specSilent {
				note = " (cell not named by the decision table; expectation is the store's documented extension)"
			}
			if got.Outcome != tc.wantOut {
				t.Errorf("outcome = %q, want %q%s", got.Outcome, tc.wantOut, note)
			}
			if got.ConflictReason != tc.wantReason {
				t.Errorf("conflict reason = %q, want %q%s", got.ConflictReason, tc.wantReason, note)
			}
			if got.PriorState != tc.seed {
				t.Errorf("PriorState = %q, want %q", got.PriorState, tc.seed)
			}
			if got.Outcome.MayDispatch() != (tc.wantOut == ReserveNew || tc.wantOut == ReserveReclaimed) {
				t.Errorf("MayDispatch() = %v for outcome %q", got.Outcome.MayDispatch(), got.Outcome)
			}

			// Replay must hand back the cached response verbatim.
			if tc.wantOut == ReserveReplay {
				if got.ResponseStatus != 200 || got.ResponseBody != `{"dispatched":true}` {
					t.Errorf("replay returned %d / %q, want the cached 200 response", got.ResponseStatus, got.ResponseBody)
				}
				if got.EventID != "evt_seed" {
					t.Errorf("replay EventID = %q, want the original evt_seed", got.EventID)
				}
			}

			// The protocol writes NO trigger event for replay, in_progress or
			// conflict: those forwarded nothing and must not enter the
			// resolver queue or the coverage join.
			wantTriggers := 1
			if tc.wantOut == ReserveReclaimed {
				wantTriggers = 2
			}
			if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'trigger'`); n != wantTriggers {
				t.Errorf("got %d trigger events, want %d for outcome %q", n, wantTriggers, got.Outcome)
			}
		})
	}
}

// TestTokenRotationKeepsIdempotencyNamespace: rotating a caller's token must
// not break idempotency.
//
// The idempotency namespace is the caller NAME. A token rotation tok_a1 ->
// tok_a2 must still find the caller's own record: a replay, never a conflict
// and never a fresh dispatch. caller_token_id is audit trail only.
func TestTokenRotationKeepsIdempotencyNamespace(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	path := "POST /t/acme/core/release.yml"

	first := reserveReq("acme", "key-rot", path, body, "evt_rot_1")
	first.CallerTokenID = "tok_a1"
	first.Event.CallerTokenID = "tok_a1"
	if res, err := env.Reserve(c, first); err != nil || res.Outcome != ReserveNew {
		t.Fatalf("Reserve under tok_a1 = %v, %v; want new", res.Outcome, err)
	}
	if _, err := env.Complete(c, completeReq("acme", "key-rot", "evt_rot_1", StateComplete, 200, `{"dispatched":true}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Same caller_name, rotated token.
	second := reserveReq("acme", "key-rot", path, body, "evt_rot_2")
	second.CallerTokenID = "tok_a2"
	second.Event.CallerTokenID = "tok_a2"
	res, err := env.Reserve(c, second)
	if err != nil {
		t.Fatalf("Reserve under tok_a2: %v", err)
	}
	if res.Outcome != ReserveReplay {
		t.Fatalf("outcome after token rotation = %q (reason %q), want replay — the namespace is keyed on caller_name", res.Outcome, res.ConflictReason)
	}
	if res.ResponseStatus != 200 {
		t.Errorf("replay status = %d, want 200", res.ResponseStatus)
	}
	// The stored audit token is the one that took the reservation; the replay
	// does not rewrite it.
	rec, err := env.LookupIdempotency(c, "acme", "key-rot")
	if err != nil {
		t.Fatalf("LookupIdempotency: %v", err)
	}
	if rec.CallerTokenID != "tok_a1" {
		t.Errorf("caller_token_id = %q, want the reserving token tok_a1", rec.CallerTokenID)
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'trigger'`); n != 1 {
		t.Errorf("got %d trigger events, want 1 — a replay must not log a dispatch", n)
	}

	// A different caller_name with the same key is a different namespace
	// entirely.
	other := reserveReq("beta", "key-rot", path, body, "evt_rot_3")
	if res, err := env.Reserve(c, other); err != nil || res.Outcome != ReserveNew {
		t.Fatalf("Reserve for caller beta = %v, %v; want new (keys are per caller)", res.Outcome, err)
	}
}

// TestTokenRotationOnReclaim checks the reservation CAS path writes the rotated
// token onto the row, since that UPDATE is the one place caller_token_id is
// rewritten.
func TestTokenRotationOnReclaim(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	path := "POST /t/acme/core/release.yml"

	first := reserveReq("acme", "key-rot2", path, body, "evt_r1")
	first.CallerTokenID = "tok_a1"
	if _, err := env.Reserve(c, first); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := env.Complete(c, completeReq("acme", "key-rot2", "evt_r1", StateUnknown, 0, "")); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	second := reserveReq("acme", "key-rot2", path, body, "evt_r2")
	second.CallerTokenID = "tok_a2"
	res, err := env.Reserve(c, second)
	if err != nil {
		t.Fatalf("retry Reserve: %v", err)
	}
	if res.Outcome != ReserveReclaimed {
		t.Fatalf("outcome = %q, want reclaimed after a token rotation", res.Outcome)
	}
	rec, err := env.LookupIdempotency(c, "acme", "key-rot2")
	if err != nil {
		t.Fatalf("LookupIdempotency: %v", err)
	}
	if rec.CallerTokenID != "tok_a2" {
		t.Errorf("caller_token_id = %q after reclaim, want the rotated tok_a2", rec.CallerTokenID)
	}
}

// TestCompleteUnknownStoresNoResponse: a timeout or 5xx caches
// nothing. A cached body for a result the gateway never observed would be
// replayed to the next caller as if it were authoritative.
func TestCompleteUnknownStoresNoResponse(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	if _, err := env.Reserve(c, reserveReq("acme", "key-unk", "POST /p", bodyHash("b"), "evt_unk")); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// The caller passes a status and a body anyway — the store must drop both.
	req := completeReq("acme", "key-unk", "evt_unk", StateUnknown, 502, `{"error":"bad_gateway"}`)
	res, err := env.Complete(c, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !res.Applied {
		t.Fatalf("Complete was not applied to the row it owns")
	}

	var status sql.NullInt64
	var body sql.NullString
	var state string
	env.rawScan(t, []any{&state, &status, &body},
		`SELECT state, response_status, response_body FROM idempotency WHERE caller_name = ? AND idempotency_key = ?`,
		"acme", "key-unk")
	if state != string(StateUnknown) {
		t.Errorf("state = %q, want unknown", state)
	}
	if status.Valid {
		t.Errorf("response_status = %d, want SQL NULL (no unobserved result is cached)", status.Int64)
	}
	if body.Valid {
		t.Errorf("response_body = %q, want SQL NULL (no unobserved result is cached)", body.String)
	}
	// The outcome event is still written: the call really happened.
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'trigger_outcome' AND trigger_event_id = ?`, "evt_unk"); n != 1 {
		t.Errorf("got %d trigger_outcome rows, want 1", n)
	}
}

// TestCompleteRejectsInvalidFinalState: only complete and unknown are legal
// terminal states. 'in_flight' as a final state would strand the key.
func TestCompleteRejectsInvalidFinalState(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	if _, err := env.Reserve(c, reserveReq("acme", "k", "POST /p", bodyHash("b"), "evt_c")); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for _, bad := range []State{StateInFlight, State(""), State("done")} {
		if _, err := env.Complete(c, completeReq("acme", "k", "evt_c", bad, 200, "{}")); err == nil {
			t.Errorf("Complete accepted final state %q", bad)
		}
	}
	if _, err := env.Complete(c, CompleteRequest{CallerName: "acme", IdempotencyKey: "k", FinalState: StateComplete}); err == nil {
		t.Errorf("Complete accepted an empty EventID")
	}
}

// TestPurgeExpiredIdempotency covers the TTL purge. expires_at is wall-clock, so the test
// also pins the direction of the clock-jump hazard the store documents: a
// backward jump keeps records alive, a forward jump expires them early.
//
// And it pins the clock-jump guard's asymmetry, which is a real behaviour and not an accident:
// every run persists ITS OWN wall time, including a run whose clock
// moved backwards. The stamp therefore goes backwards too, and the bill for a
// backward jump is presented to the NEXT run, which sees a huge apparent gap
// and skips. The test asserts that skip explicitly, because a reader who has
// does not know that a long stop is indistinguishable from a jump will
// otherwise read it as a bug.
func TestPurgeExpiredIdempotency(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	req := reserveReq("acme", "key-ttl", "POST /p", bodyHash("b"), "evt_ttl")
	req.TTL = time.Hour
	if _, err := env.Reserve(c, req); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, err := env.Complete(c, completeReq("acme", "key-ttl", "evt_ttl", StateComplete, 200, "{}")); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// First run on this database: no stamp exists, so no jump check is made —
	// only the stamp is written.
	env.clock.Set(testBase().Add(time.Hour)) // expires_at == now, cutoff is strict <
	if n, err := env.PurgeExpiredIdempotency(c); err != nil || n != 0 {
		t.Fatalf("purge at expires_at = %d rows, err %v; want 0", n, err)
	}

	// Clock jumps BACKWARD. That is the safe direction for expires_at — records
	// only live longer — and the elapsed-since-stamp value is negative, so the
	// guard does not fire. The run purges normally and stamps its own (earlier)
	// wall time, which is required of every run.
	env.clock.Set(testBase().Add(-24 * time.Hour))
	if n, err := env.PurgeExpiredIdempotency(c); err != nil || n != 0 {
		t.Fatalf("purge after a backward clock jump = %d rows, err %v; want 0 (the safe direction)", n, err)
	}

	// Clock returns to real time. From the stamp's point of view 25 hours have
	// passed, far beyond the threshold of ten purge periods, so this run is
	// indistinguishable from a FORWARD jump and is skipped — the guard's cost, paid by
	// the run AFTER a backward jump rather than by the jump itself.
	before := env.ClockJumpSuspectedTotal()
	env.clock.Set(testBase().Add(time.Hour + time.Millisecond))
	n, err := env.PurgeExpiredIdempotency(c)
	if !errors.Is(err, ErrClockJumpSuspected) {
		t.Fatalf("purge after a 25h apparent gap = %d rows, err %v; want ErrClockJumpSuspected", n, err)
	}
	if n != 0 {
		t.Errorf("a suspected clock jump deleted %d rows, want 0 — the skip must be total", n)
	}
	if got := env.ClockJumpSuspectedTotal(); got != before+1 {
		t.Errorf("ClockJumpSuspectedTotal = %d, want %d — the counter is what makes this loud rather than silent", got, before+1)
	}
	if _, err := env.LookupIdempotency(c, "acme", "key-ttl"); err != nil {
		t.Fatalf("the record was deleted by a run that reported a clock jump: %v", err)
	}

	// The guard is one-shot: the stamp advanced even though nothing was
	// deleted, so the very next run at the same wall time purges normally.
	// Without that, a protection that fired once would stop the purge forever
	// and the table would grow without bound.
	if n, err := env.PurgeExpiredIdempotency(c); err != nil || n != 1 {
		t.Fatalf("purge after the skipped run = %d rows, err %v; want 1 — the guard must not latch", n, err)
	}
	if got := env.ClockJumpSuspectedTotal(); got != before+1 {
		t.Errorf("ClockJumpSuspectedTotal = %d after a normal run, want %d", got, before+1)
	}
	if _, err := env.LookupIdempotency(c, "acme", "key-ttl"); !errors.Is(err, ErrNotFound) {
		t.Errorf("record survived the purge: %v", err)
	}
	// The trigger log is NOT touched by the TTL purge — event retention has its own
	// retention.
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events`); n != 2 {
		t.Errorf("got %d events after the idempotency purge, want 2 (trigger + outcome)", n)
	}
}
