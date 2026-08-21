package store

import (
	"errors"
	"testing"
	"time"
)

func sampleRun(id int64, conclusion string) GitHubRun {
	return GitHubRun{
		RunID:      id,
		Repo:       "acme/core",
		WorkflowID: 99,
		Workflow:   "release.yml",
		Event:      "workflow_dispatch",
		HeadBranch: "main",
		Actor:      "standpipe-gateway",
		Conclusion: conclusion,
		CreatedAt:  testBase(),
	}
}

// TestUpsertGitHubRunsConclusionChanges is the run_observed production rule: a
// run_observed is produced only when a run's conclusion is non-NULL AND changed
// AND the run carries an active run_link.
//
// `status` and `conclusion` are different fields; `status` is stored
// NOWHERE — github_runs has no such column and GitHubRun no such field,
// deliberately — and "in_progress" is a `status` value that is written into no
// conclusion column, field or example.
//
// WHY THERE IS NO "in_progress" CASE HERE, so nobody adds one back:
//
//   - An earlier version of this test drove ” -> "in_progress" -> "success"
//     and asserted the middle step produced no change. It failed, and it looked
//     like a store defect. It was not.
//   - The store's skip condition is `conclusion == "" || conclusion == prev`.
//     That IS the production rule ("non-NULL" and "changed"), exactly and completely: a run
//     that has not concluded arrives with an EMPTY conclusion, and the first
//     clause already drops it. No code change was ever required.
//   - The failing step was feeding "in_progress" into GitHubRun.Conclusion — an
//     input GitHub cannot produce and the rule forbids. Making it pass would have
//     required a special case naming "in_progress" in the conclusion column,
//     reintroducing precisely the status/conclusion confusion the rule was
//     written to remove, so the step was removed.
//
// The sequence below is the real one an inventory refresh sees: an empty
// conclusion for as long as the run is queued or running, then the conclusion.
func TestUpsertGitHubRunsConclusionChanges(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	trig := env.seedTrigger(t, "evt_run_parent", "k")
	if _, err := env.AppendRunLink(c, RunLinkEvent{
		EventID: "evt_run_link", TriggerEventID: trig, RunID: runID(500), Resolution: ResolutionExact,
		ResolutionSource: SourceRunName,
	}); err != nil {
		t.Fatalf("AppendRunLink: %v", err)
	}

	// 1. First sight: no conclusion at all.
	changes, err := env.UpsertGitHubRuns(c, []GitHubRun{sampleRun(500, "")})
	if err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("first sight with no conclusion reported %v, want no change", changes)
	}

	// 2. Still running, seen a second time. No `status` is stored at all, so a
	// run that has not concluded looks EXACTLY like this on every refresh: an
	// empty conclusion, indefinitely. Repeating it is the real polling shape,
	// and rule 3 must hold every time rather than only on first sight.
	changes, err = env.UpsertGitHubRuns(c, []GitHubRun{sampleRun(500, "")})
	if err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("a second refresh with no conclusion reported %v, want no change", changes)
	}

	// 3. Concluded.
	changes, err = env.UpsertGitHubRuns(c, []GitHubRun{sampleRun(500, "success")})
	if err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("NULL -> success reported %d changes, want 1", len(changes))
	}
	if changes[0].RunID != 500 || changes[0].To != "success" || changes[0].TriggerEventID != trig {
		t.Errorf("change = %+v, want run 500 -> success on trigger %s", changes[0], trig)
	}
	// From is the PREVIOUS conclusion, and there was none. It must be empty,
	// not a stand-in value: the rule forbids representing "had not concluded" with
	// any conclusion string at all, on the way in or on the way out.
	if changes[0].From != "" {
		t.Errorf("change.From = %q, want empty — the run had no previous conclusion", changes[0].From)
	}

	// 4. Re-seeing the same conclusion is not a change.
	changes, err = env.UpsertGitHubRuns(c, []GitHubRun{sampleRun(500, "success")})
	if err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("re-upserting an unchanged conclusion reported %v, want no change", changes)
	}
}

// TestUpsertGitHubRunsWithoutRunLink: the same transitions on a run nothing
// links to. A run_link is required for the event to be produced — otherwise
// there is no trigger to attach the run_observed to.
func TestUpsertGitHubRunsWithoutRunLink(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	// "" twice, then a conclusion: the same legal sequence as above. No
	// "in_progress" here either — that value is kept out of every conclusion
	// example, and a fixture is an example.
	for _, conclusion := range []string{"", "", "success"} {
		changes, err := env.UpsertGitHubRuns(c, []GitHubRun{sampleRun(600, conclusion)})
		if err != nil {
			t.Fatalf("UpsertGitHubRuns(%q): %v", conclusion, err)
		}
		if len(changes) != 0 {
			t.Errorf("conclusion %q on an unlinked run reported %v, want no change", conclusion, changes)
		}
	}
	// The cache row is still written: coverage needs the history even
	// with no link.
	got, err := env.GitHubRunConclusion(c, 600)
	if err != nil {
		t.Fatalf("GitHubRunConclusion: %v", err)
	}
	if got != "success" {
		t.Errorf("cached conclusion = %q, want success", got)
	}
	if _, err := env.ActiveRunLink(c, 600); !errors.Is(err, ErrNotFound) {
		t.Errorf("ActiveRunLink(600) = %v, want ErrNotFound", err)
	}
	if _, err := env.GitHubRunConclusion(c, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GitHubRunConclusion(999) = %v, want ErrNotFound", err)
	}
}

// TestUpsertGitHubRunsValidation and normalisation.
func TestUpsertGitHubRunsNormalisation(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	if changes, err := env.UpsertGitHubRuns(c, nil); err != nil || changes != nil {
		t.Errorf("UpsertGitHubRuns(nil) = %v, %v; want nil, nil", changes, err)
	}
	if _, err := env.UpsertGitHubRuns(c, []GitHubRun{{Repo: "acme/core"}}); err == nil {
		t.Errorf("UpsertGitHubRuns accepted a run with run_id 0")
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM github_runs`); n != 0 {
		t.Errorf("%d rows written by a rejected batch, want 0 — the batch is one transaction", n)
	}

	if _, err := env.UpsertGitHubRuns(c, []GitHubRun{sampleRun(700, "success")}); err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	var normalized string
	env.rawScan(t, []any{&normalized}, `SELECT head_branch_normalized FROM github_runs WHERE run_id = ?`, 700)
	if normalized != "refs/heads/main" {
		t.Errorf("head_branch_normalized = %q, want refs/heads/main", normalized)
	}

	// An explicit normalised value is not overwritten.
	r := sampleRun(701, "success")
	r.HeadBranch = "v1.2.3"
	r.HeadBranchNormalized = "refs/tags/v1.2.3"
	if _, err := env.UpsertGitHubRuns(c, []GitHubRun{r}); err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	env.rawScan(t, []any{&normalized}, `SELECT head_branch_normalized FROM github_runs WHERE run_id = ?`, 701)
	if normalized != "refs/tags/v1.2.3" {
		t.Errorf("head_branch_normalized = %q, want the caller's refs/tags/v1.2.3", normalized)
	}
}

func TestNormalizeRef(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"main":             "refs/heads/main",
		"feature/x":        "refs/heads/feature/x",
		"refs/heads/main":  "refs/heads/main",
		"refs/tags/v1":     "refs/tags/v1",
		"refs/pull/1/head": "refs/pull/1/head",
	}
	for in, want := range cases {
		if got := NormalizeRef(in); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPurgeOldRuns is the retention on the run-history cache table.
func TestPurgeOldRuns(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	old := sampleRun(800, "success")
	old.CreatedAt = testBase().Add(-100 * 24 * time.Hour)
	recent := sampleRun(801, "success")
	if _, err := env.UpsertGitHubRuns(c, []GitHubRun{old, recent}); err != nil {
		t.Fatalf("UpsertGitHubRuns: %v", err)
	}
	if _, err := env.PurgeOldRuns(c, 0); err == nil {
		t.Errorf("PurgeOldRuns accepted a zero retention")
	}
	n, err := env.PurgeOldRuns(c, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("PurgeOldRuns = %d, want 1", n)
	}
	if got := env.countRows(t, `SELECT COUNT(*) FROM github_runs`); got != 1 {
		t.Errorf("%d rows left, want 1", got)
	}
}
