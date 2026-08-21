package forge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-github/v90/github"

	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// The pre-retry resolve, and the blind spot ResolveOne documents first.
//
// "THE RUN ROW MAY NOT EXIST YET. GitHub creates the workflow_run row
//  asynchronously after accepting the dispatch. A retry that arrives seconds
//  after the original timeout can search a window in which the run is real but
//  not yet listable."
//
// That is not a hypothetical. It is the ONE case where ResolveOne returns a
// confident VerdictNoRun that is wrong, and the consequence is a second
// dispatch into a workflow that is already running — the failure the no-retry rule
// exists to prevent. The fake makes the window explicit and configurable so both
// sides of it can be tested.
// ---------------------------------------------------------------------------

// dispatchOnce plans and dispatches one trigger, returning the trigger event id
// and the instant of the dispatch.
func (e *env) dispatchOnce(repo, workflow, ref, eventID string) (int64, time.Time) {
	e.t.Helper()
	r, err := ParseRepo(repo)
	if err != nil {
		e.t.Fatalf("ParseRepo: %v", err)
	}
	req := DispatchRequest{
		Repo: r, Workflow: workflow, Ref: ref,
		Inputs: map[string]string{}, TriggerEventID: eventID,
	}
	plan, err := e.c.Plan(context.Background(), req)
	if err != nil {
		e.t.Fatalf("Plan: %v", err)
	}
	res, err := e.c.Dispatch(context.Background(), req, plan)
	if err != nil {
		e.t.Fatalf("Dispatch: %v", err)
	}
	return res.RunID, e.clock.Now()
}

func TestResolveOneAcrossTheAsynchronousRunRow(t *testing.T) {
	const delay = 90 * time.Second

	e := newEnv(t, nil)
	e.seed("acme/core", "release.yml", workflowYAML)
	e.gh.SetRunAppearDelay(delay)

	_, dispatchedAt := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_original")
	r := NewResolver(e.c)

	pending := PendingTrigger{
		EventID:  "evt_retry",
		Repo:     "acme/core",
		Workflow: "release.yml",
		Ref:      "refs/heads/main",
		At:       dispatchedAt,
		// The RETRY's armament. The original injected standby_trigger_id, so
		// tier 2 is available — this is the arrangement that survives a lost
		// dispatch response.
		Correlation: CorrelationInjected,
	}
	window := ResolveWindow{
		AttemptAt:  dispatchedAt,
		DispatchAt: &dispatchedAt,
		// NOT pending.EventID. The reclaim CAS replaced idempotency.event_id
		// with the retry's event, and the retry's id was never injected
		// anywhere; searching for it could only miss.
		OriginalEventID: "evt_original",
	}

	t.Run("inside the invisibility window ResolveOne returns a WRONG negative", func(t *testing.T) {
		e.clock.Set(dispatchedAt.Add(10 * time.Second))
		out, err := r.ResolveOne(context.Background(), pending, window)
		if err != nil {
			t.Fatalf("ResolveOne: %v", err)
		}
		if out.Tier2 != Tier2Searched {
			t.Errorf("Tier2 = %q, want %q — the original injected an id, so the exact search must run",
				out.Tier2, Tier2Searched)
		}
		if out.CandidatesListed != 0 {
			t.Fatalf("the fake listed %d run(s) inside the delay; the async window is not being simulated",
				out.CandidatesListed)
		}
		// THIS IS THE BLIND SPOT, PINNED. The run exists on GitHub. The
		// listing does not show it yet. ResolveOne answers "no run", which
		// authorises the retry to dispatch a SECOND time.
		if out.Verdict != VerdictNoRun || out.Reason != ReasonNoCandidate {
			t.Errorf("verdict/reason = %q/%q, want no_run/no_candidate.\n"+
				"If this changed, ResolveOne learned to distinguish \"not created yet\" from "+
				"\"never dispatched\" — update the blind-spot note on ResolveOne, because it is "+
				"no longer true.", out.Verdict, out.Reason)
		}
		if !out.Verdict.SafeToDispatch() {
			t.Error("SafeToDispatch() disagrees with VerdictNoRun")
		}
		if out.Verdict.RunAlreadyExists() {
			t.Error("RunAlreadyExists() is true for VerdictNoRun")
		}
	})

	t.Run("once the row is listable tier 2 proves the run", func(t *testing.T) {
		e.clock.Set(dispatchedAt.Add(delay + time.Second))
		out, err := r.ResolveOne(context.Background(), pending, window)
		if err != nil {
			t.Fatalf("ResolveOne: %v", err)
		}
		if out.Verdict != VerdictRunFound || out.Reason != ReasonRunNameMatch {
			t.Fatalf("verdict/reason = %q/%q, want run_found/run_name_match (candidates listed: %d)",
				out.Verdict, out.Reason, out.CandidatesListed)
		}
		if out.Verdict.SafeToDispatch() {
			t.Error("SafeToDispatch() is true for a trigger whose run already exists")
		}
		if out.Link.TriggerEventID != "evt_retry" {
			t.Errorf("link.trigger_event_id = %q, want the RETRY's event — the pre-retry guard completes THAT record as "+
				"forwarded; the superseded event's outcome is `error` and no correlation query selects it",
				out.Link.TriggerEventID)
		}
		if out.Link.Resolution != ResolutionExact || out.Link.ResolutionSource != ResolutionSourceRunName {
			t.Errorf("link = %+v, want exact/run_name", out.Link)
		}
	})
}

// TestResolveOneWithoutTier2FallsToInference is blind spot 2: with tier 2 not
// armed the only evidence is "a workflow_dispatch run of this workflow on this
// ref, inside the window", which anybody else's dispatch also satisfies. The
// link says so — `inferred`, source `window` — rather than claiming proof.
func TestResolveOneWithoutTier2FallsToInference(t *testing.T) {
	e := newEnv(t, nil)
	e.seed("acme/core", "test.yml", workflowYAMLNoTriggerInput)

	_, dispatchedAt := e.dispatchOnce("acme/core", "test.yml", "refs/heads/main", "evt_original")
	e.clock.Advance(time.Second)

	r := NewResolver(e.c)
	out, err := r.ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "test.yml",
		Ref: "refs/heads/main", At: dispatchedAt,
		Correlation: CorrelationDispatchResponse,
	}, ResolveWindow{AttemptAt: dispatchedAt, DispatchAt: &dispatchedAt})
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if out.Tier2 != Tier2NotArmed {
		t.Errorf("Tier2 = %q, want %q — nothing was injected, so no run name can carry a trigger id",
			out.Tier2, Tier2NotArmed)
	}
	if out.Verdict != VerdictRunFound || out.Reason != ReasonWindowMatch {
		t.Fatalf("verdict/reason = %q/%q, want run_found/window_match", out.Verdict, out.Reason)
	}
	if out.Link.Resolution != ResolutionInferred || out.Link.ResolutionSource != ResolutionSourceWindow {
		t.Errorf("link = %+v, want inferred/window — a time-window match is an INFERENCE and the "+
			"record must not claim otherwise", out.Link)
	}
}

// TestResolveOneReportsTier2ArmedButUnusable pins the degradation the pre-retry guard names:
// tier 2 WAS armed and the caller could not supply the id that was injected.
// The exact search is skipped rather than run against the wrong id, and the
// fact is REPORTED (no degradation may be silent) instead of being folded into "no match".
func TestResolveOneReportsTier2ArmedButUnusable(t *testing.T) {
	e := newEnv(t, nil)
	e.seed("acme/core", "release.yml", workflowYAML)
	_, dispatchedAt := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_original")
	e.clock.Advance(time.Second)

	r := NewResolver(e.c)
	out, err := r.ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "release.yml",
		Ref: "refs/heads/main", At: dispatchedAt, Correlation: CorrelationInjected,
	}, ResolveWindow{
		AttemptAt: dispatchedAt, DispatchAt: &dispatchedAt,
		OriginalEventID: "", // the caller could not supply it
	})
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if out.Tier2 != Tier2OriginalIDUnavailable {
		t.Errorf("Tier2 = %q, want %q", out.Tier2, Tier2OriginalIDUnavailable)
	}
	// It degrades to tier 3 and says the answer is inferred.
	if out.Link.Resolution == ResolutionExact {
		t.Error("a link was claimed `exact` while the exact search never ran")
	}
}

// TestResolveOneTrustsNoDispatchWithoutCallingGitHub is blind spot 3's other
// half: MarkDispatchStarted commits BEFORE the POST, so a nil DispatchAt means
// nothing was posted. With tier 2 also unavailable there is nothing an API
// call could establish, and spending one to acquire evidence we would have to
// discard is waste.
func TestResolveOneTrustsNoDispatchWithoutCallingGitHub(t *testing.T) {
	e := newEnv(t, nil)
	e.seed("acme/core", "test.yml", workflowYAMLNoTriggerInput)
	before := e.gh.Calls(testutil.OpListRuns)

	r := NewResolver(e.c)
	out, err := r.ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "test.yml",
		Ref: "refs/heads/main", At: e.clock.Now(), Correlation: CorrelationDispatchResponse,
	}, ResolveWindow{AttemptAt: e.clock.Now(), DispatchAt: nil})
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if out.Verdict != VerdictNoRun || out.Reason != ReasonNeverDispatched {
		t.Errorf("verdict/reason = %q/%q, want no_run/never_dispatched", out.Verdict, out.Reason)
	}
	if got := e.gh.Calls(testutil.OpListRuns) - before; got != 0 {
		t.Errorf("%d run listing(s) issued for a trigger that never dispatched", got)
	}
}

// TestResolveOneRefusesToClaimANegativeFromATruncatedPage is blind spot 4. A
// full page means the listing was CUT, not that the repo was quiet, and a
// negative claimed from it authorises a duplicate dispatch on an answer that
// was never computed.
func TestResolveOneRefusesToClaimANegativeFromATruncatedPage(t *testing.T) {
	e := newEnv(t, nil)
	workflowID := e.seed("acme/core", "release.yml", workflowYAML)

	at := e.clock.Now()
	for i := 0; i < perPage; i++ {
		e.gh.AddRun(testutil.Run{
			Repo: "acme/core", WorkflowID: workflowID, Path: ".github/workflows/release.yml",
			Event: "workflow_dispatch",
			// A ref nobody asked about: the page is full and yet no candidate
			// matches, which is exactly the shape that would otherwise read as
			// "there is no run".
			HeadBranch: "somebody-elses-branch",
			RunName:    "release",
			CreatedAt:  at,
		})
	}

	r := NewResolver(e.c)
	out, err := r.ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "release.yml",
		Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
	}, ResolveWindow{AttemptAt: at, DispatchAt: &at, OriginalEventID: "evt_original"})

	if !errors.Is(err, ErrResolveInconclusive) {
		t.Fatalf("err = %v, want ErrResolveInconclusive", err)
	}
	if out.Verdict != VerdictUnknown || out.Reason != ReasonCandidatesTruncated {
		t.Errorf("verdict/reason = %q/%q, want unknown/candidates_truncated", out.Verdict, out.Reason)
	}
	// Neither predicate is true for VerdictUnknown, on purpose: a two-branch
	// if/else at the call site cannot route the unknown case into an action.
	if out.Verdict.SafeToDispatch() || out.Verdict.RunAlreadyExists() {
		t.Error("VerdictUnknown answered one of the two decision predicates")
	}
}

// TestResolveOneIsInconclusiveWhenGitHubIsUnreachable: the pre-retry check is a GUARD, so an
// unreachable GitHub is inconclusive and never a negative.
func TestResolveOneIsInconclusiveWhenGitHubIsUnreachable(t *testing.T) {
	e := newEnv(t, nil)
	e.seed("acme/core", "release.yml", workflowYAML)
	at := e.clock.Now()
	e.gh.Fail(testutil.OpListRuns, testutil.FailServerError)

	r := NewResolver(e.c)
	out, err := r.ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "release.yml",
		Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
	}, ResolveWindow{AttemptAt: at, DispatchAt: &at, OriginalEventID: "evt_original"})

	if !errors.Is(err, ErrResolveInconclusive) {
		t.Fatalf("err = %v, want ErrResolveInconclusive", err)
	}
	if out.Verdict != VerdictUnknown {
		t.Errorf("verdict = %q, want unknown — an unreachable GitHub is NOT evidence of absence", out.Verdict)
	}
}

// ---------------------------------------------------------------------------
// The background sweep, and the unmapped-Correlation hazard at forge's end:
// a PendingTrigger whose Correlation the adapter dropped reads as "tier 2 not
// armed" and silently downgrades exact links.
// ---------------------------------------------------------------------------

// TestSweepRoutesByCorrelation is the observable consequence of the adapter
// dropping PendingTrigger.Correlation: an injected trigger that arrives with a
// zero Correlation takes the time-window path and comes back `inferred`
// instead of `exact`. Nothing errors and nothing logs.
func TestSweepRoutesByCorrelation(t *testing.T) {
	cases := []struct {
		name       string
		corr       Correlation
		wantRes    Resolution
		wantSource ResolutionSource
	}{
		{"injected reaches tier 2", CorrelationInjected, ResolutionExact, ResolutionSourceRunName},
		{"dispatch_response falls to tier 3", CorrelationDispatchResponse, ResolutionInferred, ResolutionSourceWindow},
		{
			// The unmapped-Correlation hazard, made visible. A dropped field is not
			// "unknown": it reads as "tier 2 not armed".
			"a DROPPED correlation silently downgrades exact to inferred",
			Correlation(""), ResolutionInferred, ResolutionSourceWindow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.seed("acme/core", "release.yml", workflowYAML)
			_, at := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_original")
			e.clock.Advance(time.Second)

			e.store.mu.Lock()
			e.store.pending = []PendingTrigger{{
				EventID: "evt_original", Repo: "acme/core", Workflow: "release.yml",
				Ref: "refs/heads/main", At: at, Correlation: tc.corr,
			}}
			e.store.mu.Unlock()

			stats, err := NewResolver(e.c).Sweep(context.Background())
			if err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			if stats.Pending != 1 {
				t.Fatalf("pending = %d, want 1", stats.Pending)
			}
			links := e.store.Links()
			if len(links) != 1 {
				t.Fatalf("want one run_link written, got %d (stats %+v)", len(links), stats)
			}
			if links[0].Resolution != tc.wantRes || links[0].ResolutionSource != tc.wantSource {
				t.Errorf("run_link = %s/%s, want %s/%s",
					links[0].Resolution, links[0].ResolutionSource, tc.wantRes, tc.wantSource)
			}
		})
	}
}

// TestRunNameSeamMatchesWhateverTheFakeWrites is the run-name seam guard, and
// it replaces one that had gone dormant.
//
// THE OLD GUARD AND WHY IT STOPPED WORKING. It compared two constants —
// forge.RunNameSource and testutil.RunNameFieldName — because the fake wrote
// the run-name into ONE field and the WORKFLOW name into the other, so moving
// one constant without the other turned a real tier-2 failure into a green
// test. When RunNameSource became RunNameFieldEither the test began to
// t.Skip: reading both fields, the reasoning went, means the seam cannot
// disagree. The reasoning is right and the effect was that NOTHING checked the
// seam any more, under any setting.
//
// WHAT IS PINNED INSTEAD. The measurement of 2026-08-21 (runname.go) says
// GitHub writes the run-name into `name` AND `display_title` as the same
// string, and the fake now does the same. So "the two constants agree" is no
// longer the invariant. The invariant is:
//
//	WHATEVER THE FAKE WRITES, THE MATCHER FINDS.
//
// which holds under every value of RunNameSource and therefore never has a
// reason to skip. It is checked three ways, none of them a constant
// comparison: the field names come from the fake, the reading comes from
// forge, and the JSON in between is deserialised by the same library the
// client uses.
func TestRunNameSeamMatchesWhateverTheFakeWrites(t *testing.T) {
	// The fields forge admits under the CURRENT RunNameSource. The table is
	// closed over the enumeration on purpose: a RunNameField added without an
	// entry here fails loudly instead of quietly reducing this test to a
	// tautology.
	admits := map[RunNameField][]string{
		RunNameFieldDisplayTitle: {"display_title"},
		RunNameFieldName:         {"name"},
		RunNameFieldEither:       {"display_title", "name"},
	}
	forgeFields, known := admits[RunNameSource]
	if !known {
		t.Fatalf("RunNameSource = %d has no entry in this test's table. A RunNameField was added and the "+
			"seam guard does not know which JSON fields it reads; fill in the table rather than "+
			"letting the run-name seam go unchecked.", RunNameSource)
	}

	// 1. Every field forge reads is a field the fake publishes. The reverse is
	//    allowed — a narrower RunNameSource simply ignores one of them.
	published := map[string]bool{}
	for _, f := range testutil.RunNameFields() {
		published[f] = true
	}
	for _, f := range forgeFields {
		if !published[f] {
			t.Errorf("SEAM SPLIT: forge reads %q and the fake publishes the run-name into %v. "+
				"the tier-2 field was measured once, on one repo, for workflow_dispatch; "+
				"a field forge reads and GitHub's stand-in never writes makes every tier-2 test vacuous.",
				f, testutil.RunNameFields())
		}
	}

	// 2. Field by field, through real JSON: an id placed in one of the fake's
	//    fields is found exactly when RunNameSource admits that field. This is
	//    the half that executes rather than compares — it also pins that the
	//    fake's JSON NAMES land in the go-github struct members forge reads,
	//    which no constant comparison can see.
	const probeID = "evt_seam_probe_01J000000000000000000000"
	admitted := map[string]bool{}
	for _, f := range forgeFields {
		admitted[f] = true
	}
	found := 0
	for _, field := range testutil.RunNameFields() {
		body := fmt.Sprintf(`{"id":1,%q:%q}`, field, "release • "+probeID)
		var run github.WorkflowRun
		if err := json.Unmarshal([]byte(body), &run); err != nil {
			t.Fatalf("unmarshal %s: %v", body, err)
		}
		got := RunCarriesTriggerID(&run, probeID)
		if got {
			found++
		}
		if got != admitted[field] {
			t.Errorf("RunCarriesTriggerID with the id in %q alone = %v, want %v under RunNameSource %d",
				field, got, admitted[field], RunNameSource)
		}
	}
	if found == 0 {
		t.Fatalf("the fake publishes the run-name into %v and the matcher finds it in NONE of them. "+
			"Tier 2 cannot match anything the fake produces, so every test that claims a run_name "+
			"resolution is testing something else.", testutil.RunNameFields())
	}

	// 3. End to end, through the fake's HTTP surface and the resolver.
	e := newEnv(t, nil)
	e.seed("acme/core", "release.yml", workflowYAML)
	_, at := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_seam")
	e.clock.Advance(time.Second)

	e.store.mu.Lock()
	e.store.pending = []PendingTrigger{{
		EventID: "evt_seam", Repo: "acme/core", Workflow: "release.yml",
		Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
	}}
	e.store.mu.Unlock()

	if _, err := NewResolver(e.c).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	links := e.store.Links()
	if len(links) != 1 || links[0].ResolutionSource != ResolutionSourceRunName {
		t.Fatalf("tier 2 did not match a run the fake published into %v: %+v",
			testutil.RunNameFields(), links)
	}

	// 4. Displaying and matching must not diverge. RunNameOf picks ONE field
	//    for the log line while RunCarriesTriggerID searches all of them, and
	//    the 2026-08-21 measurement is that both fields carry the same string
	//    — so the displayed value carries the id too. If this ever fails, a
	//    trace will name a run-name that is not the one correlation matched
	//    on, and the operator has no way to tell.
	out, err := NewResolver(e.c).ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "release.yml",
		Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
	}, ResolveWindow{AttemptAt: at, DispatchAt: &at, OriginalEventID: "evt_seam"})
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if out.Run == nil {
		t.Fatalf("ResolveOne matched nothing through the run-name: %+v", out)
	}
	if name := RunNameOf(out.Run); !strings.Contains(name, "evt_seam") {
		t.Errorf("RunNameOf = %q, which does not carry the id tier 2 matched on. The field forge "+
			"DISPLAYS and the fields it MATCHES on have diverged.", name)
	}
}

// ---------------------------------------------------------------------------
// The run-name GitHub declined to apply (measured 2026-08-21).
//
// This is the shape that silently kills tier 2: the id was injected, the run
// exists, the workflow declares `run-name:` — and GitHub publishes it in
// NEITHER field, so no number of fields read can match. The trigger falls to
// tier 3 and is written `inferred` with no error anywhere.
//
// internal/forge has two detectors for it, tier2Absent on the sweep and
// Tier2SearchedRunNameAbsent on the pre-retry guard, and until now nothing tested
// either against the behaviour it exists for. The fake reproduces it on
// request: testutil.GitHub.SetRunNameApplied(false).
// ---------------------------------------------------------------------------

// TestSweepCountsTheRunNameGitHubDidNotApply pins both arms: the counter fires
// on the measured absence, and it does NOT fire when the run-name is applied.
// A detector that cannot stay quiet is not a detector.
func TestSweepCountsTheRunNameGitHubDidNotApply(t *testing.T) {
	cases := []struct {
		name          string
		applied       bool
		wantAbsent    int
		wantRes       Resolution
		wantSource    ResolutionSource
		wantEmissions int
	}{
		{
			"applied: tier 2 proves the link and the counter stays silent",
			true, 0, ResolutionExact, ResolutionSourceRunName, 0,
		},
		{
			// The degradation. Everything else about this trigger looks like
			// a success: the dispatch was forwarded, a run exists, a link is
			// written. Only the counter says the proof was lost.
			"NOT applied: the link degrades to inferred and the counter is the only signal",
			false, 1, ResolutionInferred, ResolutionSourceWindow, 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, nil)
			e.seed("acme/core", "release.yml", workflowYAML)
			e.gh.SetRunNameApplied(tc.applied)

			_, at := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_original")
			// PAST THE HORIZON. Inside it, "no match" is indistinguishable
			// from "the run does not exist yet" and the resolver correctly
			// claims nothing (tier2Pending). The absence only becomes
			// evidence once waiting longer cannot help.
			e.clock.Advance(DefaultResolverHorizon + time.Second)

			e.store.mu.Lock()
			e.store.pending = []PendingTrigger{{
				EventID: "evt_original", Repo: "acme/core", Workflow: "release.yml",
				Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
			}}
			e.store.mu.Unlock()

			stats, err := NewResolver(e.c).Sweep(context.Background())
			if err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			if stats.RunNameAbsent != tc.wantAbsent {
				t.Errorf("SweepStats.RunNameAbsent = %d, want %d (stats %+v)",
					stats.RunNameAbsent, tc.wantAbsent, stats)
			}
			links := e.store.Links()
			if len(links) != 1 {
				t.Fatalf("want one run_link, got %d (stats %+v)", len(links), stats)
			}
			if links[0].Resolution != tc.wantRes || links[0].ResolutionSource != tc.wantSource {
				t.Errorf("run_link = %s/%s, want %s/%s",
					links[0].Resolution, links[0].ResolutionSource, tc.wantRes, tc.wantSource)
			}

			samples := e.metrics.samples("gateway_run_name_absent_total")
			if len(samples) != tc.wantEmissions {
				t.Fatalf("gateway_run_name_absent_total emitted %d time(s), want %d. This counter is the "+
					"ONLY trace a lost tier-2 proof leaves.", len(samples), tc.wantEmissions)
			}
			if tc.wantEmissions > 0 {
				got := samples[0].labels
				if got["repo"] != "acme/core" || got["workflow"] != "release.yml" {
					t.Errorf("labels = %v, want repo=acme/core workflow=release.yml — the diagnosis is a "+
						"RATE per repo/workflow, so unlabelled emissions cannot be read", got)
				}
			}
		})
	}
}

// TestResolveOneReportsTheRunNameGitHubDidNotApply is the same measured
// behaviour on the pre-retry guard, where the consequence is worse than a
// weaker record: the retry is SUPPRESSED on a tier-3 inference for a trigger
// that was supposed to be provable. Tier2SearchedRunNameAbsent is the only
// place that difference is visible, and ResolveOne emits no metric of its own,
// so the caller's log line is the whole of it.
func TestResolveOneReportsTheRunNameGitHubDidNotApply(t *testing.T) {
	e := newEnv(t, nil)
	e.seed("acme/core", "release.yml", workflowYAML)
	e.gh.SetRunNameApplied(false)

	_, at := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_original")
	e.clock.Advance(time.Second)

	before := len(e.metrics.samples("gateway_run_name_absent_total"))
	out, err := NewResolver(e.c).ResolveOne(context.Background(), PendingTrigger{
		EventID: "evt_retry", Repo: "acme/core", Workflow: "release.yml",
		Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
	}, ResolveWindow{AttemptAt: at, DispatchAt: &at, OriginalEventID: "evt_original"})
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}
	if out.CandidatesListed == 0 {
		t.Fatalf("the fake listed no runs; without a candidate this test cannot distinguish an absent "+
			"run-name from an absent run (outcome %+v)", out)
	}
	if out.Tier2 != Tier2SearchedRunNameAbsent {
		t.Errorf("Tier2 = %q, want %q. The run-name search ran, the window held a run on the right ref, "+
			"and no run carried the injected id — which is exactly what this status exists to say.",
			out.Tier2, Tier2SearchedRunNameAbsent)
	}
	if out.Verdict != VerdictRunFound || out.Reason != ReasonWindowMatch {
		t.Fatalf("verdict/reason = %q/%q, want run_found/window_match", out.Verdict, out.Reason)
	}
	if out.Link.Resolution != ResolutionInferred || out.Link.ResolutionSource != ResolutionSourceWindow {
		t.Errorf("link = %+v, want inferred/window — the proof was lost, and the record must not claim it",
			out.Link)
	}
	// ResolveOne's contract: nothing is written, not to the store and not to
	// the registry. The counter belongs to the caller that owns the
	// completion write.
	if got := len(e.metrics.samples("gateway_run_name_absent_total")) - before; got != 0 {
		t.Errorf("ResolveOne emitted gateway_run_name_absent_total %d time(s); it is documented to emit "+
			"nothing, and a guard that writes metrics double-counts with the sweep", got)
	}
}
