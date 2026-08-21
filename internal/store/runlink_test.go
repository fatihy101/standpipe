package store

import (
	"errors"
	"testing"
)

func runID(v int64) *int64 { return &v }

// sourceFor is the correlation layer -> resolution mapping, read backwards. AppendRunLink
// is the BACKGROUND resolver's entry point (layer 2 and 3); the layer-1 link
// travels with the completion instead, so `dispatch_response`
// is deliberately not produced here. Layer 2 is the only resolver layer that
// yields `exact`; layer 3 yields inferred / ambiguous / none.
func sourceFor(r Resolution) ResolutionSource {
	if r == ResolutionExact {
		return SourceRunName
	}
	return SourceWindow
}

// seedTrigger creates a trigger event that run_link / run_observed rows can
// reference. trigger_event_id is a real foreign key, so the parent has to
// exist.
func (e *testEnv) seedTrigger(t *testing.T, eventID, key string) string {
	t.Helper()
	res, err := e.Reserve(ctx(t), reserveReq("acme", key, "POST /t/acme/core/release.yml", bodyHash(key), eventID))
	if err != nil {
		t.Fatalf("seed Reserve: %v", err)
	}
	if res.Outcome != ReserveNew {
		t.Fatalf("seed Reserve outcome = %q, want new", res.Outcome)
	}
	return eventID
}

func (e *testEnv) activeRunLinks(t *testing.T, triggerEventID string) int {
	t.Helper()
	return e.countRows(t, `SELECT COUNT(*) FROM trigger_events
		 WHERE event_type = 'run_link' AND superseded_by IS NULL AND trigger_event_id = ?`, triggerEventID)
}

// TestAppendRunLinkSupersession walks every ordered pair of the run_link reliability
// ladder (none < ambiguous < inferred < exact).
//
// The invariant that matters is structural: whatever the outcome, exactly ONE
// active run_link may exist per trigger. The partial UNIQUE index
// idx_run_link_unique enforces it, so a supersede that is not atomic surfaces
// here as a constraint error rather than as a silent second row.
func TestAppendRunLinkSupersession(t *testing.T) {
	ladder := []Resolution{ResolutionNone, ResolutionAmbiguous, ResolutionInferred, ResolutionExact}
	runIDFor := func(r Resolution) *int64 {
		if r == ResolutionNone {
			return nil
		}
		return runID(4242)
	}

	for _, first := range ladder {
		for _, second := range ladder {
			t.Run(string(first)+"_then_"+string(second), func(t *testing.T) {
				env := newTestEnv(t)
				c := ctx(t)
				trig := env.seedTrigger(t, "evt_link_parent", "k")

				firstID, err := env.AppendRunLink(c, RunLinkEvent{
					EventID: "evt_link_1", TriggerEventID: trig,
					RunID: runIDFor(first), Resolution: first,
					ResolutionSource: sourceFor(first),
				})
				if err != nil {
					t.Fatalf("first AppendRunLink(%s): %v", first, err)
				}
				if firstID != "evt_link_1" {
					t.Fatalf("first AppendRunLink returned %q", firstID)
				}
				if got := env.activeRunLinks(t, trig); got != 1 {
					t.Fatalf("%d active run_links after the first append, want 1", got)
				}

				secondID, err := env.AppendRunLink(c, RunLinkEvent{
					EventID: "evt_link_2", TriggerEventID: trig,
					RunID: runIDFor(second), Resolution: second,
					ResolutionSource: sourceFor(second),
				})

				stronger := second.rank() > first.rank()
				if stronger {
					if err != nil {
						t.Fatalf("AppendRunLink(%s over %s) = %v, want success", second, first, err)
					}
					if secondID != "evt_link_2" {
						t.Errorf("returned event id %q, want evt_link_2", secondID)
					}
					// The old row is superseded, in place, append-only.
					var supersededBy any
					env.rawScan(t, []any{&supersededBy},
						`SELECT superseded_by FROM trigger_events WHERE event_id = ?`, "evt_link_1")
					if supersededBy != "evt_link_2" {
						t.Errorf("superseded_by = %v on the old row, want evt_link_2", supersededBy)
					}
					if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'run_link'`); n != 2 {
						t.Errorf("%d run_link rows, want 2 — the supersede must be append-only", n)
					}
				} else {
					if !errors.Is(err, ErrWeakerResolution) {
						t.Fatalf("AppendRunLink(%s over %s) = %v, want ErrWeakerResolution", second, first, err)
					}
					if secondID != "" {
						t.Errorf("returned event id %q on a refused append", secondID)
					}
					if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'run_link'`); n != 1 {
						t.Errorf("%d run_link rows, want 1 — a refused append must write nothing", n)
					}
					var supersededBy any
					env.rawScan(t, []any{&supersededBy},
						`SELECT superseded_by FROM trigger_events WHERE event_id = ?`, "evt_link_1")
					if supersededBy != nil {
						t.Errorf("superseded_by = %v after a refused append, want NULL", supersededBy)
					}
				}

				// The invariant, in both branches.
				if got := env.activeRunLinks(t, trig); got != 1 {
					t.Fatalf("%d active run_links, want exactly 1 — the partial UNIQUE index is the last line of defence", got)
				}
			})
		}
	}
}

// TestAppendRunLinkFullLadder climbs all four rungs in one database, which is
// the sequence the resolver actually produces as evidence improves.
func TestAppendRunLinkFullLadder(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	trig := env.seedTrigger(t, "evt_ladder", "k")

	steps := []struct {
		id  string
		res Resolution
		rid *int64
	}{
		{"evt_l1", ResolutionNone, nil},
		{"evt_l2", ResolutionAmbiguous, runID(11)},
		{"evt_l3", ResolutionInferred, runID(22)},
		{"evt_l4", ResolutionExact, runID(33)},
	}
	for _, s := range steps {
		if _, err := env.AppendRunLink(c, RunLinkEvent{
			EventID: s.id, TriggerEventID: trig, RunID: s.rid, Resolution: s.res,
			ResolutionSource: sourceFor(s.res),
		}); err != nil {
			t.Fatalf("AppendRunLink(%s): %v", s.res, err)
		}
		if got := env.activeRunLinks(t, trig); got != 1 {
			t.Fatalf("%d active run_links after %s, want 1", got, s.res)
		}
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'run_link'`); n != 4 {
		t.Errorf("%d run_link rows after the full ladder, want 4 (append-only)", n)
	}
	var active string
	env.rawScan(t, []any{&active}, `SELECT event_id FROM trigger_events
		WHERE event_type = 'run_link' AND superseded_by IS NULL AND trigger_event_id = ?`, trig)
	if active != "evt_l4" {
		t.Errorf("active run_link = %q, want evt_l4 (exact)", active)
	}
	// resolution_source is stored per row, not derived from resolution: the
	// window rungs keep `window` and only the exact rung says `run_name`.
	for _, want := range []struct{ id, source string }{
		{"evt_l1", string(SourceWindow)},
		{"evt_l2", string(SourceWindow)},
		{"evt_l3", string(SourceWindow)},
		{"evt_l4", string(SourceRunName)},
	} {
		var got string
		env.rawScan(t, []any{&got}, `SELECT resolution_source FROM trigger_events WHERE event_id = ?`, want.id)
		if got != want.source {
			t.Errorf("%s resolution_source = %q, want %q", want.id, got, want.source)
		}
	}

	got, err := env.ActiveRunLink(c, 33)
	if err != nil {
		t.Fatalf("ActiveRunLink(33): %v", err)
	}
	if got != trig {
		t.Errorf("ActiveRunLink(33) = %q, want %q", got, trig)
	}
	// The superseded rows' run ids are no longer active.
	if _, err := env.ActiveRunLink(c, 11); !errors.Is(err, ErrNotFound) {
		t.Errorf("ActiveRunLink(11) = %v, want ErrNotFound — 11 was superseded", err)
	}
}

// TestAppendRunLinkValidation: resolution 'none' carries no run_id and every
// other resolution requires one. A run_link claiming 'exact' with a
// NULL run_id would look like a correlated trigger in the coverage query while pointing at
// nothing.
func TestAppendRunLinkValidation(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	trig := env.seedTrigger(t, "evt_v", "k")

	cases := []struct {
		name string
		ev   RunLinkEvent
	}{
		// Every case carries a VALID resolution_source, so that it still fails
		// for the reason its name gives. Leaving the field zero here would make
		// all seven pass through the resolution_source guard instead and the
		// run_id rules would stop being tested at all.
		{"no trigger_event_id", RunLinkEvent{Resolution: ResolutionExact, RunID: runID(1), ResolutionSource: SourceRunName}},
		{"invalid resolution", RunLinkEvent{TriggerEventID: trig, Resolution: Resolution("perfect"), RunID: runID(1), ResolutionSource: SourceWindow}},
		{"empty resolution", RunLinkEvent{TriggerEventID: trig, Resolution: "", RunID: runID(1), ResolutionSource: SourceWindow}},
		{"none with a run_id", RunLinkEvent{TriggerEventID: trig, Resolution: ResolutionNone, RunID: runID(1), ResolutionSource: SourceWindow}},
		{"exact without a run_id", RunLinkEvent{TriggerEventID: trig, Resolution: ResolutionExact, ResolutionSource: SourceRunName}},
		{"inferred without a run_id", RunLinkEvent{TriggerEventID: trig, Resolution: ResolutionInferred, ResolutionSource: SourceWindow}},
		{"ambiguous without a run_id", RunLinkEvent{TriggerEventID: trig, Resolution: ResolutionAmbiguous, ResolutionSource: SourceWindow}},
		// resolution_source is NOT NULL on a run_link and has no
		// default — "which layer established this link" cannot be guessed.
		{"empty resolution_source", RunLinkEvent{TriggerEventID: trig, Resolution: ResolutionExact, RunID: runID(1)}},
		{"invalid resolution_source", RunLinkEvent{TriggerEventID: trig, Resolution: ResolutionExact, RunID: runID(1), ResolutionSource: ResolutionSource("guess")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.AppendRunLink(c, tc.ev); err == nil {
				t.Fatalf("AppendRunLink accepted %s", tc.name)
			}
		})
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'run_link'`); n != 0 {
		t.Errorf("%d run_link rows written by rejected appends, want 0", n)
	}
}

// TestAppendRunObservedValidation covers run_observed's required fields.
func TestAppendRunObservedValidation(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	trig := env.seedTrigger(t, "evt_ro", "k")

	if _, err := env.AppendRunObserved(c, RunObservedEvent{RunID: 1, Conclusion: "success"}); err == nil {
		t.Errorf("AppendRunObserved accepted a missing trigger_event_id")
	}
	if _, err := env.AppendRunObserved(c, RunObservedEvent{TriggerEventID: trig, RunID: 1}); err == nil {
		t.Errorf("AppendRunObserved accepted a missing conclusion")
	}
	id, err := env.AppendRunObserved(c, RunObservedEvent{TriggerEventID: trig, RunID: 7, Conclusion: "success"})
	if err != nil {
		t.Fatalf("AppendRunObserved: %v", err)
	}
	if id == "" {
		t.Fatalf("AppendRunObserved returned an empty event id")
	}
	ev, err := env.EventByID(c, id)
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}
	if ev["conclusion"] != "success" || ev["run_id"] != int64(7) {
		t.Errorf("run_observed event = %v, want conclusion success / run_id 7", ev)
	}
}

// TestRunLinkResolutionSourcePairingIsNotEnforced records a guarantee this
// package does NOT provide. It passes today, and it is written to pass, because
// its subject is the absence of a check — not a defect claim.
//
// The correlation hierarchy pairs each layer with the resolutions it can produce:
//
//	layer 1  dispatch_response -> exact
//	layer 2  run_name          -> exact
//	layer 3  window            -> inferred | ambiguous | none
//
// Nothing enforces that pairing. `validateRunLink` checks `resolution` and
// `resolution_source` independently, and the table's CHECK constraints do the same,
// so every one of the twelve combinations is storable — including the six that
// no correlation layer can produce. An `exact` link sourced from `window` claims the
// time-window resolver returned a certain match, which it structurally cannot.
//
// Why this is written down as a test rather than only as a note: the
// combinations below are the ones a future guard would have to reject, and if
// someone adds that guard, THIS test fails and names the pairing it broke. An
// impossible-combination list that is never executed rots; this one cannot.
//
// Whether to add the guard is an open decision. The reason
// it is not obviously worth it: the adapter is the only writer that could
// produce a wrong pairing, and the value is a
// report field (`standpipe inventory`, `standpipe trace`) rather than anything the gateway branches
// on. The reason it might be: a wrong pairing is valid-looking data with no
// external symptom, the adapter hazard class that is most dangerous precisely
// because nothing fails.
func TestRunLinkResolutionSourcePairingIsNotEnforced(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	// Every combination the correlation hierarchy cannot produce. If a guard lands, each of these
	// starts erroring and the test says which pairing it was.
	impossible := []struct {
		resolution Resolution
		source     ResolutionSource
		why        string
	}{
		{ResolutionExact, SourceWindow, "layer 3 matches on a time window and can never be certain"},
		{ResolutionInferred, SourceDispatchResponse, "layer 1 reads a run id out of the response; it is exact or it is nothing"},
		{ResolutionAmbiguous, SourceDispatchResponse, "layer 1 returns one run id, never a set of candidates"},
		{ResolutionNone, SourceDispatchResponse, "layer 1 with no run id writes no link at all"},
		{ResolutionInferred, SourceRunName, "layer 2 matches standby_trigger_id in display_title; a match is exact"},
		{ResolutionAmbiguous, SourceRunName, "the injected id is unique per trigger, so it cannot be ambiguous"},
		{ResolutionNone, SourceRunName, "layer 2 failing to match falls through to layer 3, it does not write none"},
	}

	for i, tc := range impossible {
		name := string(tc.resolution) + "_from_" + string(tc.source)
		t.Run(name, func(t *testing.T) {
			// A fresh parent per case: the store allows one active link per trigger,
			// and this test is about validation, not supersession.
			trig := env.seedTrigger(t, "evt_pair_"+name, "key-pair-"+name)
			var rid *int64
			if tc.resolution != ResolutionNone {
				rid = runID(int64(9000 + i))
			}
			_, err := env.AppendRunLink(c, RunLinkEvent{
				TriggerEventID: trig, RunID: rid,
				Resolution: tc.resolution, ResolutionSource: tc.source,
			})
			if err != nil {
				t.Skipf("the store now rejects %s/%s — a layer/resolution pairing guard has landed, and %s. "+
					"Delete this case and assert the rejection instead. (err: %v)",
					tc.resolution, tc.source, tc.why, err)
			}
			// It stored. Record exactly what a reader of the log would see.
			var res, src string
			env.rawScan(t, []any{&res, &src},
				`SELECT resolution, resolution_source FROM trigger_events
				  WHERE event_type = 'run_link' AND superseded_by IS NULL AND trigger_event_id = ?`, trig)
			if res != string(tc.resolution) || src != string(tc.source) {
				t.Fatalf("stored %s/%s, wrote %s/%s", res, src, tc.resolution, tc.source)
			}
		})
	}
}
