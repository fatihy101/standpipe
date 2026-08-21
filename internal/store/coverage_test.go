package store

import (
	"testing"
	"time"
)

const covRepo = "acme/core"

// seedCoverageRun writes one github_runs row for the coverage join.
func (e *testEnv) seedCoverageRun(t *testing.T, id int64, workflow, event string, createdAt time.Time) {
	t.Helper()
	if _, err := e.UpsertGitHubRuns(ctx(t), []GitHubRun{{
		RunID:      id,
		Repo:       covRepo,
		WorkflowID: 99,
		Workflow:   workflow,
		Event:      event,
		HeadBranch: "main",
		Conclusion: "success",
		CreatedAt:  createdAt,
	}}); err != nil {
		t.Fatalf("UpsertGitHubRuns(%d): %v", id, err)
	}
}

// seedCoverageTrigger writes a trigger for `workflow` at `ts` and completes it.
//
// forwarded selects which outcome_status the trigger_outcome carries. That is
// the whole point: the coverage query's filter cannot sit on the trigger row, whose
// outcome_status is structurally NULL, so it is an EXISTS
// against the child event. A trigger whose outcome is `error` must NOT count as
// a forwarded dispatch.
//
// httpStatus is recorded as written, never normalised: with tier 1 armed a
// successful dispatch answers 200, not 204, and any query keying on the number
// instead of on outcome_status returns nothing.
func (e *testEnv) seedCoverageTrigger(t *testing.T, eventID, workflow string, ts time.Time, forwarded bool, httpStatus int, link *RunLinkEvent) {
	t.Helper()
	c := ctx(t)
	key := "key-" + eventID
	req := reserveReq("acme", key, "POST /t/"+covRepo+"/"+workflow, bodyHash(eventID), eventID)
	req.Event.TS = ts
	req.Event.TargetWorkflow = workflow
	if _, err := e.Reserve(c, req); err != nil {
		t.Fatalf("Reserve(%s): %v", eventID, err)
	}
	final := StateComplete
	if !forwarded {
		final = StateUnknown
	}
	comp := completeReq("acme", key, eventID, final, httpStatus, "{}")
	comp.Outcome.EventID = eventID + "_out"
	comp.Outcome.TS = ts
	comp.RunLink = link
	if _, err := e.Complete(c, comp); err != nil {
		t.Fatalf("Complete(%s): %v", eventID, err)
	}
}

// TestCoverageCaseLadder drives every branch of the coverage query's coverage_state CASE and
// both gap_class values, plus the ±60s join window at its exact edges.
//
// It is the query behind `standpipe inventory`'s coverage gap — one of the four
// things Phase 0 has to deliver — and it is the query that has
// already returned zero rows forever once. Its failure mode is silence:
// a wrong predicate does not error, it reports a clean sheet.
func TestCoverageCaseLadder(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	now := testBase()
	env.clock.Set(now)

	type want struct {
		state    string
		gapClass string
	}
	expect := map[string]want{}

	// --- covered: resolution 'exact', from either layer that can produce it.
	//
	// Layer 1 writes its link on the REQUEST path, in the completion's
	// transaction, and answers HTTP 200 rather than 204. The coverage query keys
	// on resolution, so both layers land in the same bucket — that is the point
	// of resolution_source being a separate field.
	env.seedCoverageRun(t, 1001, "covered-tier1.yml", "workflow_dispatch", now)
	env.seedCoverageTrigger(t, "evt_cov_t1", "covered-tier1.yml", now, true, 200,
		&RunLinkEvent{EventID: "evt_cov_t1_link", RunID: runID(1001)}) // defaults to exact/dispatch_response
	expect["covered-tier1.yml"] = want{CoverageCovered, ""}

	env.seedCoverageRun(t, 1002, "covered-tier2.yml", "workflow_dispatch", now)
	env.seedCoverageTrigger(t, "evt_cov_t2", "covered-tier2.yml", now, true, 204, nil)
	if _, err := env.AppendRunLink(c, RunLinkEvent{
		EventID: "evt_cov_t2_link", TriggerEventID: "evt_cov_t2",
		RunID: runID(1002), Resolution: ResolutionExact, ResolutionSource: SourceRunName,
	}); err != nil {
		t.Fatalf("AppendRunLink(tier 2): %v", err)
	}
	expect["covered-tier2.yml"] = want{CoverageCovered, ""}

	// --- unknown: a link exists but is weaker than exact.
	for _, tc := range []struct {
		workflow string
		id       string
		runID    int64
		res      Resolution
	}{
		{"unknown-inferred.yml", "evt_cov_inf", 1003, ResolutionInferred},
		{"unknown-ambiguous.yml", "evt_cov_amb", 1004, ResolutionAmbiguous},
	} {
		env.seedCoverageRun(t, tc.runID, tc.workflow, "workflow_dispatch", now)
		env.seedCoverageTrigger(t, tc.id, tc.workflow, now, true, 204, nil)
		if _, err := env.AppendRunLink(c, RunLinkEvent{
			EventID: tc.id + "_link", TriggerEventID: tc.id,
			RunID: runID(tc.runID), Resolution: tc.res, ResolutionSource: SourceWindow,
		}); err != nil {
			t.Fatalf("AppendRunLink(%s): %v", tc.res, err)
		}
		expect[tc.workflow] = want{CoverageUnknown, ""}
	}

	// --- unknown via resolution 'none'.
	//
	// A `none` link carries NO run_id — the run_link rules forbid one and validateRunLink
	// enforces it — so the rl join (`rl.run_id = gr.run_id`) cannot match it and
	// rl.resolution comes back NULL. The row lands on the `te.event_id IS NOT
	// NULL` branch, not on the `rl.resolution = 'none'` branch. Same answer,
	// different route. See the report: that CASE arm is unreachable.
	env.seedCoverageRun(t, 1005, "unknown-none.yml", "workflow_dispatch", now)
	env.seedCoverageTrigger(t, "evt_cov_none", "unknown-none.yml", now, true, 204, nil)
	if _, err := env.AppendRunLink(c, RunLinkEvent{
		EventID: "evt_cov_none_link", TriggerEventID: "evt_cov_none",
		Resolution: ResolutionNone, ResolutionSource: SourceWindow,
	}); err != nil {
		t.Fatalf("AppendRunLink(none): %v", err)
	}
	expect["unknown-none.yml"] = want{CoverageUnknown, ""}

	// --- unknown: a forwarded trigger sits in the window but nothing linked it.
	env.seedCoverageRun(t, 1006, "unknown-no-link.yml", "workflow_dispatch", now)
	env.seedCoverageTrigger(t, "evt_cov_nolink", "unknown-no-link.yml", now, true, 204, nil)
	expect["unknown-no-link.yml"] = want{CoverageUnknown, ""}

	// --- gap: the gateway has no record of this run at all.
	env.seedCoverageRun(t, 1007, "gap-migratable.yml", "workflow_dispatch", now)
	expect["gap-migratable.yml"] = want{CoverageGap, GapMigratable}

	// repository_dispatch is not something this gateway can take over, so its
	// gap is structural rather than migratable.
	env.seedCoverageRun(t, 1008, "gap-structural.yml", "repository_dispatch", now)
	expect["gap-structural.yml"] = want{CoverageGap, GapStructural}

	// --- the ±60s join window, at the exact edge on both sides.
	//
	// BETWEEN is inclusive, so a trigger exactly 60s away joins and one a
	// millisecond further out does not. This is the one number in the coverage query that
	// decides whether a real dispatch is counted as a gap.
	for _, tc := range []struct {
		workflow string
		id       string
		runID    int64
		offset   time.Duration
		joins    bool
	}{
		{"boundary-early-in.yml", "evt_cov_bei", 1009, -60 * time.Second, true},
		{"boundary-early-out.yml", "evt_cov_beo", 1010, -60*time.Second - time.Millisecond, false},
		{"boundary-late-in.yml", "evt_cov_bli", 1011, 60 * time.Second, true},
		{"boundary-late-out.yml", "evt_cov_blo", 1012, 60*time.Second + time.Millisecond, false},
	} {
		env.seedCoverageRun(t, tc.runID, tc.workflow, "workflow_dispatch", now)
		env.seedCoverageTrigger(t, tc.id, tc.workflow, now.Add(tc.offset), true, 204, nil)
		if tc.joins {
			expect[tc.workflow] = want{CoverageUnknown, ""}
		} else {
			expect[tc.workflow] = want{CoverageGap, GapMigratable}
		}
	}

	// --- a trigger that was NOT forwarded must not count.
	//
	// Its trigger_outcome carries outcome_status='error', so the coverage query's EXISTS does
	// not match and the run is a gap. If the filter were written on the trigger
	// row's own outcome_status — always NULL — nothing would ever match and
	// every run in the table would be reported as a gap.
	env.seedCoverageRun(t, 1013, "not-forwarded.yml", "workflow_dispatch", now)
	env.seedCoverageTrigger(t, "evt_cov_err", "not-forwarded.yml", now, false, 0, nil)
	expect["not-forwarded.yml"] = want{CoverageGap, GapMigratable}

	// --- a run this gateway could never have produced is out of scope entirely.
	// It IS in github_runs — the exclusion is the coverage query's WHERE, not a failed insert.
	env.seedCoverageRun(t, 1014, "excluded-push.yml", "push", now)
	if n := env.countRows(t, `SELECT COUNT(*) FROM github_runs WHERE run_id = ?`, 1014); n != 1 {
		t.Fatalf("the push run was not cached at all; the exclusion below would prove nothing")
	}

	if _, err := env.Coverage(c, 0); err == nil {
		t.Errorf("Coverage accepted a zero historyDays")
	}
	rows, err := env.Coverage(c, 30)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}

	got := map[string]want{}
	counts := map[string]int64{}
	for _, r := range rows {
		if r.Repo != covRepo {
			t.Errorf("unexpected repo %q", r.Repo)
		}
		if _, dup := got[r.Workflow]; dup {
			t.Errorf("%s produced two rows; each fixture is one run and must group into one", r.Workflow)
		}
		got[r.Workflow] = want{r.CoverageState, r.GapClass}
		counts[r.Workflow] = r.RunCount
		if !r.LastSeen.Equal(now) {
			t.Errorf("%s last_seen = %s, want %s", r.Workflow, r.LastSeen, now)
		}
	}

	if _, ok := got["excluded-push.yml"]; ok {
		t.Errorf("a `push` run appeared in the coverage report; the coverage query counts only workflow_dispatch and repository_dispatch")
	}
	delete(got, "excluded-push.yml")

	for workflow, w := range expect {
		g, ok := got[workflow]
		if !ok {
			t.Errorf("%s is missing from the coverage report entirely", workflow)
			continue
		}
		if g != w {
			t.Errorf("%s = %s/%q, want %s/%q", workflow, g.state, g.gapClass, w.state, w.gapClass)
		}
		if counts[workflow] != 1 {
			t.Errorf("%s run_count = %d, want 1", workflow, counts[workflow])
		}
	}
	for workflow := range got {
		if _, ok := expect[workflow]; !ok {
			t.Errorf("unexpected workflow %q in the coverage report", workflow)
		}
	}

	// gap_class is meaningful ONLY on a gap row. A non-NULL class on a covered
	// or unknown row would be counted by any consumer that groups on it.
	for workflow, g := range got {
		if g.state != CoverageGap && g.gapClass != "" {
			t.Errorf("%s is %s but carries gap_class %q, want empty", workflow, g.state, g.gapClass)
		}
		if g.state == CoverageGap && g.gapClass == "" {
			t.Errorf("%s is a gap with no gap_class", workflow)
		}
	}

	// The two percentages are separate gauges over the same total.
	s := Summarise(rows)
	// 13, not 14: the `push` run never enters the report.
	if s.Total != 13 {
		t.Errorf("Summarise total = %d, want 13 runs", s.Total)
	}
	if s.CoveredCount != 2 {
		t.Errorf("covered = %d, want 2 (tier 1 + tier 2)", s.CoveredCount)
	}
	if s.UnknownCount != 6 {
		t.Errorf("unknown = %d, want 6 (inferred, ambiguous, none, no-link, 2 in-window boundaries)", s.UnknownCount)
	}
	if s.GapCount != 5 {
		t.Errorf("gap = %d, want 5 (migratable, structural, 2 out-of-window boundaries, not-forwarded)", s.GapCount)
	}
	if s.CoveredCount+s.UnknownCount+s.GapCount != s.Total {
		t.Errorf("the three states do not partition the total: %+v", s)
	}
}

// TestCoverageHistoryWindow: historyDays is a real cutoff on gr.created_at and
// is never hard coded. A run older than the window is not a gap, it is
// not in the report at all — the difference matters, because a gap is an action
// item and an absent row is not.
func TestCoverageHistoryWindow(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	now := testBase()
	env.clock.Set(now)

	env.seedCoverageRun(t, 2001, "recent.yml", "workflow_dispatch", now.Add(-24*time.Hour))
	env.seedCoverageRun(t, 2002, "ancient.yml", "workflow_dispatch", now.Add(-40*24*time.Hour))

	rows, err := env.Coverage(c, 30)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(rows) != 1 || rows[0].Workflow != "recent.yml" {
		t.Fatalf("Coverage(30) = %+v, want only recent.yml", rows)
	}
	rows, err = env.Coverage(c, 90)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("Coverage(90) returned %d rows, want 2", len(rows))
	}
}
