package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// IS THE FAKE FAITHFUL? — the double compared against what was MEASURED on
// real GitHub, not against what is convenient here.
//
// The four closed-enumeration mechanisms in this project all derive the code
// side from what running code produces rather than from a hand-written copy. A
// test double is the same problem one level over: internal/testutil.GitHub is
// a hand-written copy of GitHub, and until this file existed NOTHING compared
// it to GitHub. Every test in the suite that touches the upstream is only as
// true as this file's subject.
//
// The oracle is three measurements taken on 2026-08-21 against
// fatihy101/standpipe-probe with four real workflow_dispatch calls (the same
// measurements recorded in internal/forge's ref.go, runname.go and
// dispatchlatency.go):
//
//	X1  a declared run-name lands in BOTH `name` and `display_title`, as the
//	    same string, and the workflow's own `name:` is in NEITHER.
//	X2  the dispatch endpoint accepts refs/heads/main, refs/tags/v0.1 AND the
//	    bare v0.1 — all three answered 200.
//	X3  return_run_details adds no latency (~1.4 s either way); WITH it the run
//	    record exists as the response returns, WITHOUT it a listing made
//	    immediately after the 204 finds nothing and one ~2 s later finds the
//	    run.
//
// Two rules this file is written under:
//
//   - A DELIBERATE simplification is fine; an ACCIDENTAL difference a test
//     relies on is the defect. So the divergences are not deleted, they are
//     PINNED, in TestFakeKnownDivergencesFromMeasuredGitHub, each with the
//     real behaviour named in the failure message. A divergence nobody wrote
//     down is the one that surfaces in the operator's terminal during setup.
//   - Nothing here asserts a behaviour that was not measured. Where a question
//     cannot be settled without a GitHub App, the test says so instead of
//     guessing — see theListingQuestionTier1CannotSettle.
//
// ---------------------------------------------------------------------------

const fidelityWorkflowYAML = `
name: release
run-name: release ${{ inputs.standby_trigger_id }}
on:
  workflow_dispatch:
    inputs:
      standby_trigger_id:
        required: false
`

// fidelityFake returns a fake with one repo and one workflow, plus the
// workflow's numeric id.
func fidelityFake(t *testing.T) (*GitHub, int64) {
	t.Helper()
	g := NewGitHub()
	t.Cleanup(g.Close)
	g.AddRepo("acme/core", "main", 5001)
	id := g.AddWorkflow("acme/core", "release.yml", fidelityWorkflowYAML)
	return g, id
}

// dispatch posts one dispatch request body and returns the response and its raw bytes.
// The body is written as literal JSON rather than marshalled from a struct so
// that "the parameter is ABSENT" and "the parameter is false" stay two
// distinct wire shapes — go-github omits an unset *bool, and the whole of
// tier 1 hangs on that distinction.
func dispatch(t *testing.T, g *GitHub, workflow, body string) (*http.Response, []byte) {
	t.Helper()
	url := g.URL() + "repos/acme/core/actions/workflows/" + workflow + "/dispatches"
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST dispatches: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading dispatch body: %v", err)
	}
	return resp, raw
}

// listRuns reads the workflow-runs listing the resolver reads.
func listRuns(t *testing.T, g *GitHub, workflow string) []map[string]any {
	t.Helper()
	resp, err := http.Get(g.URL() + "repos/acme/core/actions/workflows/" + workflow + "/runs")
	if err != nil {
		t.Fatalf("GET runs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET runs = %d, want 200", resp.StatusCode)
	}
	var decoded struct {
		TotalCount int              `json:"total_count"`
		Runs       []map[string]any `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding runs: %v", err)
	}
	return decoded.Runs
}

func onlyRun(t *testing.T, g *GitHub, workflow string) map[string]any {
	t.Helper()
	runs := listRuns(t, g, workflow)
	if len(runs) != 1 {
		t.Fatalf("listing returned %d runs, want exactly 1", len(runs))
	}
	return runs[0]
}

func str(t *testing.T, run map[string]any, field string) string {
	t.Helper()
	v, ok := run[field]
	if !ok {
		t.Fatalf("the run object has no %q field at all; it has %v", field, keysOf(run))
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("run[%q] = %#v, want a string", field, v)
	}
	return s
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// X3 — the two dispatch regimes.
// ---------------------------------------------------------------------------

func TestFakeDispatchReproducesTheTwoMeasuredRegimes(t *testing.T) {
	t.Run("return_run_details:true answers 200 with the three measured keys", func(t *testing.T) {
		g, wfID := fidelityFake(t)
		resp, raw := dispatch(t, g, "release.yml",
			`{"ref":"refs/heads/main","inputs":{"standby_trigger_id":"evt_1"},"return_run_details":true}`)

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (with the parameter the endpoint answers 200, not 204). body=%s",
				resp.StatusCode, raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decoding %q: %v", raw, err)
		}
		// The exact key set go-github's WorkflowDispatchRunDetails carries
		// (actions_workflows.go:64-68). A missing key is a silent run id 0.
		for _, want := range []string{"workflow_run_id", "run_url", "html_url"} {
			if _, ok := body[want]; !ok {
				t.Errorf("the 200 body has no %q; it has %v", want, keysOf(body))
			}
		}
		if len(body) != 3 {
			t.Errorf("the 200 body carries %v; GitHub returns exactly the three above", keysOf(body))
		}

		sent := g.Dispatches()
		if len(sent) != 1 {
			t.Fatalf("the fake recorded %d dispatches, want 1", len(sent))
		}
		if !sent[0].ReturnRunDetails || sent[0].StatusReturned != http.StatusOK {
			t.Errorf("record = {ReturnRunDetails:%v StatusReturned:%d}, want {true 200} (status is evidence, never a success predicate)",
				sent[0].ReturnRunDetails, sent[0].StatusReturned)
		}
		if sent[0].WorkflowID != wfID {
			t.Errorf("record.WorkflowID = %d, want %d", sent[0].WorkflowID, wfID)
		}
		gotID, ok := body["workflow_run_id"].(float64)
		if !ok || int64(gotID) != sent[0].RunID {
			t.Errorf("workflow_run_id = %#v, want the recorded run %d — tier 1 reads THIS number",
				body["workflow_run_id"], sent[0].RunID)
		}
	})

	t.Run("the parameter omitted answers 204 and zero bytes", func(t *testing.T) {
		g, _ := fidelityFake(t)
		resp, raw := dispatch(t, g, "release.yml", `{"ref":"refs/heads/main","inputs":{}}`)

		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 — the measurement recorded \"204, zero bytes\" for the "+
				"parameterless call, and a fake that always returned a run id would make tier-1 code "+
				"look correct on a deployment where tier 1 is off", resp.StatusCode)
		}
		if len(raw) != 0 {
			t.Errorf("the 204 carried %d bytes (%q); the measured call carried zero", len(raw), raw)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			t.Errorf("the 204 carried Content-Type %q; a 204 has no body to type", ct)
		}
		sent := g.Dispatches()
		if len(sent) != 1 || sent[0].ReturnRunDetails || sent[0].StatusReturned != http.StatusNoContent {
			t.Errorf("record = %+v, want ReturnRunDetails false and StatusReturned 204", sent)
		}
		// The run still exists. That is the whole point of the no-retry rule: a 204 is
		// evidence the dispatch was ACCEPTED, and correlation falls to tiers
		// 2 and 3 — not evidence that nothing ran.
		if len(g.Runs()) != 1 {
			t.Errorf("the fake holds %d runs after a 204; the dispatch was accepted, so there is one",
				len(g.Runs()))
		}
	})

	t.Run("return_run_details:false is the 204 regime, not a third one", func(t *testing.T) {
		g, _ := fidelityFake(t)
		resp, raw := dispatch(t, g, "release.yml",
			`{"ref":"refs/heads/main","inputs":{},"return_run_details":false}`)
		if resp.StatusCode != http.StatusNoContent || len(raw) != 0 {
			t.Errorf("status/len = %d/%d, want 204/0: github.return_run_details:false must reach the "+
				"wire as the parameterless behaviour", resp.StatusCode, len(raw))
		}
	})

	t.Run("neither regime waits for the run to become listable", func(t *testing.T) {
		// X3 finding 1, in the only form a fake can hold: the response is not
		// blocked on the run row. DispatchLatencyModel = LatencyIndependent
		// says the parameter is not a multiplier on upstream_timeout; a fake
		// that slept until VisibleAt would model LatencyBlocking and every
		// timeout test would then be measuring the wrong thing.
		g, _ := fidelityFake(t)
		g.SetRunAppearDelay(time.Hour)

		for _, body := range []string{
			`{"ref":"refs/heads/main","inputs":{},"return_run_details":true}`,
			`{"ref":"refs/heads/main","inputs":{}}`,
		} {
			start := time.Now()
			dispatch(t, g, "release.yml", body)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("the dispatch took %s with a one-hour appear delay set; the fake is modelling "+
					"the BLOCKING latency model, which the 2026-08-21 measurement excluded", elapsed)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// X2 — the three ref forms that were measured, and the head_branch they
// produce.
// ---------------------------------------------------------------------------

func TestFakeAcceptsTheThreeMeasuredRefForms(t *testing.T) {
	// The measured ref table, verbatim. All three answered 200 on live GitHub;
	// here the acceptance shows as 204 or 200 depending only on the parameter.
	for _, ref := range []string{"refs/heads/main", "refs/tags/v0.1", "v0.1"} {
		t.Run(ref, func(t *testing.T) {
			g, _ := fidelityFake(t)
			resp, raw := dispatch(t, g, "release.yml",
				fmt.Sprintf(`{"ref":%q,"inputs":{}}`, ref))
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("ref %q -> %d (%s); the endpoint accepted all three forms on 2026-08-21",
					ref, resp.StatusCode, raw)
			}
			sent := g.Dispatches()
			if len(sent) != 1 || sent[0].Ref != ref {
				t.Fatalf("recorded ref = %+v, want %q verbatim — no normalization step exists and "+
					"none is needed (X2); a fake that rewrote the ref would hide one that appeared",
					sent, ref)
			}
		})
	}

	// The bare form is GitHub's business, not ours. The gateway REFUSES
	// a short name, and that rule is ours, for disambiguation. The fake must
	// not enforce our rule for us, or forge.IsFullRef's own tests would be
	// proving the fake instead of the validator.
	t.Run("the fake does not enforce the gateway's own full-ref rule", func(t *testing.T) {
		g, _ := fidelityFake(t)
		resp, _ := dispatch(t, g, "release.yml", `{"ref":"main","inputs":{}}`)
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("bare ref -> %d; GitHub is LOOSER than the gateway here (X2) and the double "+
				"must stay on GitHub's side of that line", resp.StatusCode)
		}
	})
}

func TestFakeReportsTheBareTagNameAsHeadBranch(t *testing.T) {
	// MEASURED 2026-08-24, read-only and unauthenticated against
	// api.github.com: tag-triggered runs report head_branch as the bare tag.
	//
	//	goreleaser/goreleaser  run 32676941256  head_branch "v2.18.0"
	//	helm/helm              run 31740535156  head_branch "v3.21.4"
	//
	// Never "refs/tags/...". forge/ref.go says the same thing in prose ("a run
	// triggered on a tag also reports the tag in head_branch, with no ref_type
	// to tell them apart") and BranchRef is written for it, so a fake that
	// emitted the full ref was contradicting the code's own stated model AND
	// feeding the normaliser a value it can never see in the field.
	for _, tc := range []struct{ ref, want string }{
		{"refs/heads/main", "main"},
		{"refs/tags/v0.1", "v0.1"},
		{"v0.1", "v0.1"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			g, _ := fidelityFake(t)
			dispatch(t, g, "release.yml", fmt.Sprintf(`{"ref":%q,"inputs":{}}`, tc.ref))
			if got := str(t, onlyRun(t, g, "release.yml"), "head_branch"); got != tc.want {
				t.Errorf("head_branch = %q for ref %q, want %q", got, tc.ref, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// X1 — the run-name fields.
// ---------------------------------------------------------------------------

func TestFakeRunNameFieldsAreTheTwoThatWereMeasured(t *testing.T) {
	got := RunNameFields()
	want := []string{"name", "display_title"}
	if len(got) != len(want) {
		t.Fatalf("RunNameFields() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RunNameFields() = %v, want %v — these are the two fields the run-name was "+
				"measured in on 2026-08-21, and writeRuns panics on a third it cannot publish",
				got, want)
		}
	}
}

func TestFakeAppliedRunNameLandsInBothFieldsAndTheWorkflowNameInNeither(t *testing.T) {
	t.Run("the spec's own recommended template", func(t *testing.T) {
		// The setup guide tells operators to write
		// run-name: "<workflow> • ${{ inputs.standby_trigger_id }}", so the
		// default template's output CONTAINS the workflow name by design.
		// That is faithful to what the document asks for; it is also why the
		// second sub-test exists.
		g, _ := fidelityFake(t)
		dispatch(t, g, "release.yml",
			`{"ref":"refs/heads/main","inputs":{"standby_trigger_id":"evt_01J8"},"return_run_details":true}`)

		run := onlyRun(t, g, "release.yml")
		name, title := str(t, run, "name"), str(t, run, "display_title")
		if name != title {
			t.Errorf("name = %q, display_title = %q; the measurement found the SAME string in both "+
				"on all four dispatches, which is what makes RunNameFieldEither free", name, title)
		}
		if !strings.Contains(name, "evt_01J8") || !strings.Contains(title, "evt_01J8") {
			t.Errorf("the injected trigger id reached neither field: name=%q display_title=%q", name, title)
		}
	})

	t.Run("a run-name disjoint from the workflow name, as the probe rig had it", func(t *testing.T) {
		// The probe rig used `name: probe-workflow-name` against
		// `run-name: probe • <id>` precisely so the two could not be confused.
		// With a template that shares nothing with the workflow name, "the
		// workflow's own name is in NEITHER field" is testable at full
		// strength instead of by suffix.
		g, _ := fidelityFake(t)
		g.SetRunNameTemplate(func(workflow, triggerID string) string {
			return "carried-" + triggerID
		})
		dispatch(t, g, "release.yml",
			`{"ref":"refs/heads/main","inputs":{"standby_trigger_id":"evt_01J8"},"return_run_details":true}`)

		run := onlyRun(t, g, "release.yml")
		name, title := str(t, run, "name"), str(t, run, "display_title")
		if name != "carried-evt_01J8" || title != "carried-evt_01J8" {
			t.Fatalf("name/display_title = %q/%q, want the run-name in both", name, title)
		}
		// The workflow's own name, as this fake derives it (see AddWorkflow's
		// known simplification).
		const workflowName = "release"
		if strings.Contains(name, workflowName) || strings.Contains(title, workflowName) {
			t.Errorf("the workflow name %q appears in name=%q display_title=%q; on all four measured "+
				"workflow_dispatch runs it appeared in neither", workflowName, name, title)
		}
	})
}

func TestFakeReproducesTheRunNameThatWasNeverApplied(t *testing.T) {
	// The third 2026-08-21 measurement, and the only shape in which tier 2
	// fails while every other signal says the dispatch succeeded. Corroborated
	// 2026-08-24 on live public runs that declare no run-name at all:
	// goreleaser run 32676941256 carries name "release" (the workflow's own
	// name) and display_title "test(dockers/v2): cover debug helpers (#6808)"
	// (the head commit subject) — exactly this fallback pair.
	g, _ := fidelityFake(t)
	g.SetRunNameApplied(false)
	dispatch(t, g, "release.yml",
		`{"ref":"refs/heads/main","inputs":{"standby_trigger_id":"evt_01J8"},"return_run_details":true}`)

	run := onlyRun(t, g, "release.yml")
	name, title := str(t, run, "name"), str(t, run, "display_title")
	if name != "release" {
		t.Errorf("name = %q, want the workflow's own name %q", name, "release")
	}
	if title != defaultHeadCommitSubject {
		t.Errorf("display_title = %q, want the head commit subject %q", title, defaultHeadCommitSubject)
	}
	if strings.Contains(name, "evt_01J8") || strings.Contains(title, "evt_01J8") {
		t.Fatalf("the trigger id is still visible (name=%q display_title=%q); this shape exists so "+
			"that gateway_run_name_absent_total has behaviour to detect", name, title)
	}
	// The template still ran. The value existed and GitHub did not publish it —
	// that is the difference between "no run-name declared" and "not applied".
	if got := g.Runs()[0].RunName; !strings.Contains(got, "evt_01J8") {
		t.Errorf("Run.RunName = %q; the template's output is still recorded, because the point of "+
			"this shape is that the template existed and the value did not appear", got)
	}
}

// ---------------------------------------------------------------------------
// THE DIVERGENCES. Each one is a place the fake is simpler than GitHub. They
// are pinned rather than deleted so that the list stays a decision instead of
// drifting into an accident — and so the reader of a green suite can see
// exactly what the green does NOT cover.
// ---------------------------------------------------------------------------

func TestFakeKnownDivergencesFromMeasuredGitHub(t *testing.T) {
	t.Run("a ref that names nothing is accepted", func(t *testing.T) {
		g, _ := fidelityFake(t)
		resp, _ := dispatch(t, g, "release.yml",
			`{"ref":"refs/heads/no-such-branch-anywhere","inputs":{}}`)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d; if the fake learned to reject a ref that does not exist, this "+
				"divergence is CLOSED — delete this sub-test and its recorded divergence", resp.StatusCode)
		}
		// REAL GITHUB: 422 {"message":"No ref found for: <ref>"}. The fake
		// holds no refs at all, so the only way to exercise KindUpstreamRejected
		// is FailUnprocessable — which means no test drives the 422 an operator
		// is most likely to cause. The setup guide's troubleshooting table names
		// exactly this case ("422 upstream_rejected ... probably the ref form").
		t.Log("recorded divergence: the fake has no refs; real GitHub answers 422 " +
			`"No ref found for: <ref>" and that path is reachable here only through FailUnprocessable`)
	})

	t.Run("an error response carries no rate headers", func(t *testing.T) {
		g, _ := fidelityFake(t)
		g.Fail(OpDispatch, FailNotFound)
		resp, _ := dispatch(t, g, "release.yml", `{"ref":"refs/heads/main","inputs":{}}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != "" {
			t.Fatalf("the 404 carried X-RateLimit-Remaining %q; real GitHub does too, so this "+
				"divergence is closed — update the memory note and let limiter.observe's failure "+
				"path be exercised", got)
		}
		// REAL GITHUB attaches the full X-RateLimit-* set to an authenticated
		// 404 or 422, so limiter.observe's failure path has never run.
	})

	t.Run("X-RateLimit-Used is never emitted, on success either", func(t *testing.T) {
		g, _ := fidelityFake(t)
		dispatch(t, g, "release.yml", `{"ref":"refs/heads/main","inputs":{}}`)
		resp, err := http.Get(g.URL() + "repos/acme/core/actions/workflows/release.yml/runs")
		if err != nil {
			t.Fatalf("GET runs: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if got := resp.Header.Get("X-RateLimit-Used"); got != "" {
			t.Fatalf("X-RateLimit-Used = %q; the fake has learned to send it, which is what real "+
				"GitHub always does — close this divergence", got)
		}
		// Nothing reads it today. That is the point: a change that STARTED
		// reading it would go green here and find nothing in the field.
	})

	t.Run("a workflow's name comes from the file name, not the file", func(t *testing.T) {
		g := NewGitHub()
		t.Cleanup(g.Close)
		g.AddRepo("acme/core", "main", 5001)
		g.AddWorkflow("acme/core", "probe.yml", "name: probe-workflow-name\non:\n  workflow_dispatch: {}\n")

		resp, err := http.Get(g.URL() + "repos/acme/core/actions/workflows")
		if err != nil {
			t.Fatalf("GET workflows: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var listing struct {
			Workflows []map[string]any `json:"workflows"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
			t.Fatalf("decoding workflows: %v", err)
		}
		if len(listing.Workflows) != 1 {
			t.Fatalf("listing has %d workflows, want 1", len(listing.Workflows))
		}
		if got := listing.Workflows[0]["name"]; got != "probe" {
			t.Fatalf("workflow name = %v; if the fake now reads the file's own `name:` this "+
				"divergence is closed and the 2026-08-21 probe rig becomes reproducible here", got)
		}
		// REAL GITHUB publishes the DECLARED name ("probe-workflow-name").
		// Consequence: the measurement rig, whose whole design was two
		// deliberately different strings, cannot be rebuilt against this fake.
		// Nothing in the gateway compares the workflow name, which is why the
		// simplification stands.
	})

	t.Run("the run-by-id endpoint does not exist here", func(t *testing.T) {
		g, _ := fidelityFake(t)
		dispatch(t, g, "release.yml", `{"ref":"refs/heads/main","inputs":{},"return_run_details":true}`)
		runID := g.Dispatches()[0].RunID

		resp, err := http.Get(fmt.Sprintf("%srepos/acme/core/actions/runs/%d", g.URL(), runID))
		if err != nil {
			t.Fatalf("GET run: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("GET runs/%d = %d; the endpoint has been implemented, so the tier-1 measurement "+
				"below can now be reproduced — do that and close it", runID, resp.StatusCode)
		}
		// This matters more than it looks. X3's tier-1 finding was made by
		// READING THE RUN BY ID immediately after the 200. That endpoint is
		// not implemented here, so the measured claim cannot be replayed
		// against the fake at all — see the next sub-test.
	})

	t.Run("theListingQuestionTier1CannotSettle", func(t *testing.T) {
		// THE ONE DIFFERENCE THAT CANNOT BE SETTLED WITHOUT A GITHUB APP.
		//
		// Measured (X3): with return_run_details the workflow_run row EXISTS
		// when the call returns — "the returned run_id was read immediately and
		// the row was there", i.e. read BY ID. Without it, a LISTING made immediately
		// after the 204 found nothing.
		//
		// The gateway's resolver only ever uses the LISTING. So the measured
		// claim ("the row exists") and the modelled behaviour ("the row is
		// listable") are not the same statement, and nobody has measured
		// whether a tier-1 dispatch's run is listable immediately.
		//
		// This fake applies RunAppearDelay to BOTH regimes. That is neither
		// confirmed nor contradicted by the measurement. It is pinned here so
		// that the assumption is visible rather than implicit.
		g, _ := fidelityFake(t)
		g.SetRunAppearDelay(time.Hour)
		dispatch(t, g, "release.yml",
			`{"ref":"refs/heads/main","inputs":{},"return_run_details":true}`)

		if len(g.Runs()) != 1 {
			t.Fatalf("the fake holds %d runs; the dispatch created one", len(g.Runs()))
		}
		if runs := listRuns(t, g, "release.yml"); len(runs) != 0 {
			t.Fatalf("the listing returned %d runs inside the appear delay. If the fake now exempts "+
				"the tier-1 regime from the delay, that is a CHOICE about an unmeasured behaviour: "+
				"say which measurement licensed it, and note that TestResolveOneAcrossTheAsynchronous"+
				"RunRow builds its window in this regime", len(runs))
		}
	})
}

func TestFakeAnswers501ForAnEndpointItDoesNotImplement(t *testing.T) {
	// The contract that keeps an uncovered endpoint from reading as success.
	g, _ := fidelityFake(t)
	resp, err := http.Get(g.URL() + "repos/acme/core/actions/caches")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 — an unrouted path must be a visible failure, never "+
			"200-with-nothing", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "GET") || !strings.Contains(msg, "/actions/caches") {
		t.Errorf("message = %q, want it to name the method and the path", msg)
	}
}
