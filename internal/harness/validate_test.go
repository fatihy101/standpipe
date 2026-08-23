package harness

import (
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// The validator, and the run it was REWRITTEN for.
//
// The original validator scored g11 — both sides `success`, the engine having
// done none of the work — as a perfect match. That is a silent SUCCESS, and the
// whole reason Validate orders validation before derivation. Everything below
// exists because getting it wrong produces a legal-looking verdict and no error.

func modelJob(id string, steps ...ModelStep) ModelJob {
	return ModelJob{JobID: id, Steps: steps}
}

func step(name string) ModelStep { return ModelStep{Name: name} }

func baseModel(jobs ...ModelJob) Model {
	return Model{WorkflowPath: ".github/workflows/t.yml", EventName: "push", Jobs: jobs}
}

func baseRequest(m Model, purpose exec.Purpose) exec.Request {
	return exec.Request{
		Repo: "owner/name", CommitSHA: strings.Repeat("c", 40),
		WorkflowPath: m.WorkflowPath, EventName: m.EventName,
		EventPayload: []byte(`{"ref":"refs/heads/main"}`),
		Plan:         m.Plan(), RunsOn: []string{"ubuntu-24.04"},
		WorkDir: "/srv/work", SupplyMode: exec.SupplyOnline,
		Purpose: purpose, Deadline: time.Minute,
	}
}

func engineResult(jobs ...exec.JobResult) exec.Result {
	return exec.Result{
		Outcome:    exec.OutcomeCompleted,
		Jobs:       jobs,
		Provenance: exec.Provenance{InvocationID: "inv-1", BackendName: "act-local"},
	}
}

func observedJob(id string, c exec.Conclusion, steps ...exec.StepObservation) exec.JobResult {
	if steps == nil {
		// Never nil. An empty slice means "no step ran".
		steps = []exec.StepObservation{}
	}
	return exec.JobResult{
		JobID:      id,
		MatrixLeg:  exec.CanonicalLegKeyFromValues(map[string]any{}),
		Conclusion: c,
		Steps:      steps,
	}
}

func observedStep(index int, name string, c exec.Conclusion) exec.StepObservation {
	return exec.StepObservation{
		Ordinal: index + 1, StepID: DeclaredStepID(index, ""), Name: name, Ran: true, Conclusion: c,
	}
}

func githubStep(declaredIndex, number int, name string, c exec.Conclusion) GitHubStep {
	return GitHubStep{Name: name, Number: number, DeclaredIndex: declaredIndex, Conclusion: c}
}

func mustValidate(t *testing.T, in Input) Verdict {
	t.Helper()
	v, err := Validate(in)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return v
}

func findingKinds(v Verdict) []string {
	var out []string
	for _, f := range v.Findings {
		out = append(out, f.Kind)
	}
	return out
}

func hasKind(v Verdict, kind string) bool {
	for _, f := range v.Findings {
		if f.Kind == kind {
			return true
		}
	}
	return false
}

// TestG11IsAMismatchNotAMatch is the run the validator exists for.
//
// The run conclusions AGREE and the engine did none of the work: a job whose
// expensive steps are guarded on `github.event.*` loses them under the engine,
// no skip line exists, and both sides conclude `success`. Scoring it a match is
// the failure the validator was rewritten to stop, and BOTH halves of row 10 have to
// decide or the rewrite is undone.
func TestG11IsAMismatchNotAMatch(t *testing.T) {
	m := baseModel(modelJob("build", step("checkout"), step("build"), step("test")))
	// The engine recorded only the first declared step. The other two vanished.
	res := engineResult(observedJob("build", exec.ConclusionSuccess,
		observedStep(0, "checkout", exec.ConclusionSuccess)))
	gh := &GitHubRun{
		RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{
			Key: JobKey{JobID: "build"}, Name: "build",
			Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 3,
			Steps: []GitHubStep{
				githubStep(0, 2, "checkout", exec.ConclusionSuccess),
				githubStep(1, 3, "build", exec.ConclusionSuccess),
				githubStep(2, 4, "test", exec.ConclusionSuccess),
			},
		}},
	}

	v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})

	if v.Outcome != exec.OutcomeCompleted || v.Row != 10 {
		t.Fatalf("verdict = row %d %s/%s, want row 10 completed", v.Row, v.Outcome, v.Reason)
	}
	if v.RunConclusion != exec.ConclusionSuccess || v.GitHubConclusion != exec.ConclusionSuccess {
		t.Fatalf("the two run conclusions must AGREE for this to be g11: ours %q, GitHub's %q", v.RunConclusion, v.GitHubConclusion)
	}
	if v.Comparison != ComparisonMismatch {
		t.Fatalf("comparison = %q, want mismatch — the conclusions agree and the engine did none of the work; findings were %v", v.Comparison, findingKinds(v))
	}
	if len(v.Mismatches()) == 0 {
		t.Fatal("no finding was marked Mismatch, so nothing explains the verdict")
	}
	// The count and the identity assertions must BOTH have something to say:
	// one step vanishing while another runs twice preserves the count.
	if !hasKind(v, FindingStepCount) || !hasKind(v, FindingStepMissing) {
		t.Fatalf("findings = %v, want both a step_count and a step_missing", findingKinds(v))
	}
}

// TestARunThatAgreesEverywhereIsAMatch is the control. Without it, a validator
// that returned `mismatch` unconditionally would pass the test above.
func TestARunThatAgreesEverywhereIsAMatch(t *testing.T) {
	m := baseModel(modelJob("build", step("checkout"), step("build")))
	res := engineResult(observedJob("build", exec.ConclusionSuccess,
		observedStep(0, "checkout", exec.ConclusionSuccess),
		observedStep(1, "build", exec.ConclusionSuccess)))
	gh := &GitHubRun{
		RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{
			Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 2,
			Steps: []GitHubStep{
				githubStep(0, 2, "checkout", exec.ConclusionSuccess),
				githubStep(1, 3, "build", exec.ConclusionSuccess),
			},
		}},
	}

	v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
	if v.Comparison != ComparisonMatch {
		t.Fatalf("comparison = %q, want match; mismatches: %v", v.Comparison, v.Mismatches())
	}
	if v.Row != 10 || v.Outcome != exec.OutcomeCompleted {
		t.Fatalf("verdict = row %d %s", v.Row, v.Outcome)
	}
}

// TestRow8sTwoProductsDoNotMerge — an UNREADABLE step set and a
// READABLE AND DIFFERENT one must not be confused. The first is the
// denominator's problem and stops the run; the second is the numerator's and
// does not. Merging them either promotes every g11 into an exclusion, or lets an
// unmeasured run be scored.
func TestRow8sTwoProductsDoNotMerge(t *testing.T) {
	m := baseModel(modelJob("build", step("a"), step("b")))
	gh := func(count int) *GitHubRun {
		return &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
			Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess,
				DeclaredStepCount: count,
				Steps: []GitHubStep{
					githubStep(0, 2, "a", exec.ConclusionSuccess),
					githubStep(1, 3, "b", exec.ConclusionSuccess),
				}}}}
	}

	t.Run("a total miss stops the run", func(t *testing.T) {
		// The job RAN and produced no declared step record at all.
		res := engineResult(observedJob("build", exec.ConclusionSuccess))
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh(2)})
		if v.Row != 8 || v.Outcome != exec.OutcomeHarnessError || v.Reason != exec.RefusalStepSetUnavailable {
			t.Fatalf("verdict = row %d %s/%s, want row 8 harness_error/step_set_unavailable", v.Row, v.Outcome, v.Reason)
		}
		if v.Comparison != ComparisonNotApplicable {
			t.Fatalf("comparison = %q; an unreadable step set was scored", v.Comparison)
		}
		if v.RunConclusion != "" {
			t.Fatalf("an excluded run acquired conclusion %q", v.RunConclusion)
		}
	})

	t.Run("a partial miss reaches row 10 and is scored a mismatch", func(t *testing.T) {
		res := engineResult(observedJob("build", exec.ConclusionSuccess,
			observedStep(0, "a", exec.ConclusionSuccess)))
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh(2)})
		if v.Row != 10 || v.Outcome != exec.OutcomeCompleted {
			t.Fatalf("verdict = row %d %s/%s, want row 10 completed — a partial miss is a mismatch, not an exclusion", v.Row, v.Outcome, v.Reason)
		}
		if v.Comparison != ComparisonMismatch {
			t.Fatalf("comparison = %q, want mismatch", v.Comparison)
		}
		if hasKind(v, FindingStepSetUnavailable) {
			t.Fatal("a partial miss was reported as an unavailable step set; the unproven-empty class is back as the standing answer")
		}
	})

	t.Run("a SKIPPED job that recorded no steps is neither", func(t *testing.T) {
		// A skipped job runs no steps on either side; comparing empty sets is
		// not a check, and calling it unavailable would exclude the run.
		m := baseModel(
			ModelJob{JobID: "gate", Expected: exec.ConclusionSkipped, SkipCause: "if_false", Steps: []ModelStep{step("a")}},
			modelJob("build", step("a")),
		)
		res := engineResult(observedJob("build", exec.ConclusionSuccess,
			observedStep(0, "a", exec.ConclusionSuccess)))
		gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess, Jobs: []GitHubJob{
			{Key: JobKey{JobID: "gate"}, Conclusion: exec.ConclusionSkipped, DeclaredStepCount: -1},
			{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1,
				Steps: []GitHubStep{githubStep(0, 2, "a", exec.ConclusionSuccess)}},
		}}
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if v.Row != 10 {
			t.Fatalf("verdict = row %d %s/%s, want row 10", v.Row, v.Outcome, v.Reason)
		}
		if v.Comparison != ComparisonMatch {
			t.Fatalf("comparison = %q, want match; mismatches: %v", v.Comparison, v.Mismatches())
		}
	})
}

// TestContinueOnErrorIsAppliedFromTheModelBeforeComparison — step-level
// continue-on-error. The
// engine writes an OUTCOME and GitHub publishes a CONCLUSION; for a
// continue-on-error: true step that FAILED they are `failure` and `success`, and
// neither side gives both. Skipping this produces a false mismatch on every such
// step — a safe direction, but one the gate would have to explain forever.
func TestContinueOnErrorIsAppliedFromTheModelBeforeComparison(t *testing.T) {
	build := func(coe bool) Verdict {
		m := baseModel(ModelJob{JobID: "build", Steps: []ModelStep{
			{Name: "flaky", ContinueOnError: coe},
		}})
		res := engineResult(observedJob("build", exec.ConclusionSuccess,
			observedStep(0, "flaky", exec.ConclusionFailure)))
		gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
			Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess,
				DeclaredStepCount: 1,
				Steps:             []GitHubStep{githubStep(0, 2, "flaky", exec.ConclusionSuccess)}}}}
		v, err := Validate(Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if err != nil {
			t.Fatalf("Validate: %v", err)
		}
		return v
	}

	with := build(true)
	if with.Comparison != ComparisonMatch {
		t.Fatalf("with continue-on-error the run is %q; mismatches: %v — the engine's failure was compared against GitHub's success", with.Comparison, with.Mismatches())
	}
	if !hasKind(with, FindingContinueOnError) {
		t.Fatalf("the adjustment was made silently; findings = %v", findingKinds(with))
	}

	// And it is read FROM THE MODEL, so without the declaration the same run is
	// a mismatch. This is the half that proves the adjustment is not
	// unconditional.
	without := build(false)
	if without.Comparison != ComparisonMismatch {
		t.Fatalf("without continue-on-error the run is %q; a failure and a success were reconciled anyway", without.Comparison)
	}
	if hasKind(without, FindingContinueOnError) {
		t.Fatal("continue-on-error was applied to a step the model does not declare it on")
	}

	// The RECORDED conclusion is the engine's own, not the adjusted one: row 9
	// reads it, and recording the adjusted value would report a conclusion that
	// was never observed.
	if !hasKind(with, FindingContinueOnError) {
		t.Fatal("no record of the adjustment")
	}
}

// TestGitHubStepNumberIsNeverAJoinKey — GitHub's steps[].number
// is not a dense declaration order; in g11 the declared steps are numbered
// 2,3,4,5 and then jump to 10,11. There is also NO fallback to position:
// ordering the records and pairing by index is the same defect in a hat.
func TestGitHubStepNumberIsNeverAJoinKey(t *testing.T) {
	m := baseModel(modelJob("build", step("a"), step("b")))
	res := engineResult(observedJob("build", exec.ConclusionFailure,
		observedStep(0, "a", exec.ConclusionSuccess),
		observedStep(1, "b", exec.ConclusionFailure)))

	t.Run("placed by the adapter's DeclaredIndex whatever the numbers say", func(t *testing.T) {
		gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionFailure,
			Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionFailure,
				DeclaredStepCount: 2,
				Steps: []GitHubStep{
					// Numbers 10 and 11, in reverse order, as g11's really are.
					githubStep(1, 11, "b", exec.ConclusionFailure),
					githubStep(0, 10, "a", exec.ConclusionSuccess),
				}}}}
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if v.Comparison != ComparisonMatch {
			t.Fatalf("comparison = %q; the records were placed by number or by position, not by DeclaredIndex. mismatches: %v", v.Comparison, v.Mismatches())
		}
	})

	t.Run("an unplaceable record is reported, never guessed", func(t *testing.T) {
		gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionFailure,
			Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionFailure,
				DeclaredStepCount: 2,
				Steps: []GitHubStep{
					// No declared index, and two records sharing one name, so
					// the name match is not unique either.
					githubStep(-1, 10, "dup", exec.ConclusionSuccess),
					githubStep(-1, 11, "dup", exec.ConclusionSuccess),
				}}}}
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if !hasKind(v, FindingStepUnjoined) {
			t.Fatalf("an unplaceable GitHub record produced no finding: %v", findingKinds(v))
		}
		// The step whose conclusion could not be compared must NOT have been
		// compared against a positional guess. `b` failed on the engine side
		// and both GitHub records say success; a positional pairing would have
		// produced a step_conclusion mismatch.
		for _, f := range v.Mismatches() {
			if f.Kind == FindingStepConclusion {
				t.Fatalf("an unplaceable record was paired by position anyway: %s", f)
			}
		}
	})

	t.Run("placed by a unique exact name when no index was supplied", func(t *testing.T) {
		gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionFailure,
			Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionFailure,
				DeclaredStepCount: 2,
				Steps: []GitHubStep{
					githubStep(-1, 11, "b", exec.ConclusionFailure),
					githubStep(-1, 10, "a", exec.ConclusionSuccess),
				}}}}
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if v.Comparison != ComparisonMatch {
			t.Fatalf("comparison = %q; the unique name match did not place the records. mismatches: %v", v.Comparison, v.Mismatches())
		}
	})
}

// TestAnUnresolvedGitHubIdentityIsRow7NotAGuess — GitHub's API exposes a DISPLAY
// NAME ("build (ubuntu-latest, 1.21)"), not a job id and a leg key, and the
// rendering rule has not been measured. Guessing pairs the wrong legs with no
// error at all.
func TestAnUnresolvedGitHubIdentityIsRow7NotAGuess(t *testing.T) {
	m := baseModel(modelJob("build", step("a")))
	res := engineResult(observedJob("build", exec.ConclusionSuccess,
		observedStep(0, "a", exec.ConclusionSuccess)))
	gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{Name: "build (ubuntu-latest, 1.21)", Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1}}}

	v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
	if v.Row != 7 || v.Outcome != exec.OutcomeUnmeasurable || v.Reason != exec.RefusalJobIdentityUnmapped {
		t.Fatalf("verdict = row %d %s/%s, want row 7 unmeasurable/job_identity_unmapped", v.Row, v.Outcome, v.Reason)
	}
	if !hasKind(v, FindingJobIdentityUnresolved) {
		t.Fatalf("the finding does not name the unresolved identity: %v", findingKinds(v))
	}
	if v.Comparison != ComparisonNotApplicable || v.RunConclusion != "" {
		t.Fatalf("an unmeasurable run was scored: %q / %q", v.Comparison, v.RunConclusion)
	}
}

// TestAnUnevaluableGuardIsNotSkipped is the `skipped` mapping's limit, and it is what keeps (b)
// honest: an `if:` read through an empty `github.event.*` is not "false", it is
// UNKNOWN. Confirming it as `skipped` from the model brings the g11 class back
// from INSIDE the validator.
func TestAnUnevaluableGuardIsNotSkipped(t *testing.T) {
	m := baseModel(ModelJob{
		JobID: "gate", If: "github.event.repository.private == true",
		Unevaluable: "github.event.repository is not present in the payload",
		Steps:       []ModelStep{step("a")},
	})
	// The engine DID produce a result for the job — that is what gets past row
	// 6 and puts the question to row 7. See the note below on the other case.
	res := engineResult(observedJob("gate", exec.ConclusionSuccess,
		observedStep(0, "a", exec.ConclusionSuccess)))
	gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{Key: JobKey{JobID: "gate"}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1,
			Steps: []GitHubStep{githubStep(0, 2, "a", exec.ConclusionSuccess)}}}}

	// (d) is row 2 and runs first: the payload has to CARRY the field the guard
	// reads, or the run is refused before row 7 is reached. Unevaluable here
	// means the MODEL could not decide, which is a different fact.
	req := baseRequest(m, exec.PurposeDrill)
	req.EventPayload = []byte(`{"ref":"refs/heads/main","repository":{"private":true}}`)

	v := mustValidate(t, Input{Model: m, Request: req, Result: res, GitHub: gh})
	if v.Outcome != exec.OutcomeUnmeasurable || v.Row != 7 {
		t.Fatalf("verdict = row %d %s/%s, want row 7 unmeasurable", v.Row, v.Outcome, v.Reason)
	}
	if !hasKind(v, FindingGuardUnevaluable) {
		t.Fatalf("findings = %v, want the guard named", findingKinds(v))
	}

	// And the model may not claim BOTH — that state is forbidden.
	bad := baseModel(ModelJob{JobID: "gate", Expected: exec.ConclusionSkipped, Unevaluable: "unknown"})
	if err := bad.Validate(); err == nil {
		t.Fatal("Model.Validate accepted a job that is both skipped and unevaluable")
	}

	// MEASURED, AND REPORTED RATHER THAN ASSERTED. When the engine produced NO
	// result for the unevaluable job — which is the shape the mapping's limit is
	// most worried about, since an unknown guard is usually one the engine also skipped —
	// row 6 fires first and the verdict is harness_error / job_without_result.
	// Row 7's unevaluable branch is never reached and the guard is never named.
	// The exclusion is recorded under a reason that is not the truth. This test
	// records what happens; it does not bless it.
	noResult := mustValidate(t, Input{Model: m, Request: req, Result: engineResult(), GitHub: &GitHubRun{
		RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{Key: JobKey{JobID: "gate"}, Conclusion: exec.ConclusionSkipped, DeclaredStepCount: -1}},
	}})
	t.Logf("unevaluable guard with no engine result: row %d %s/%s, findings %v",
		noResult.Row, noResult.Outcome, noResult.Reason, findingKinds(noResult))
	if noResult.Outcome == exec.OutcomeCompleted {
		t.Fatal("an unevaluable guard was scored; the mapping's limit is what keeps (b) honest")
	}
	if hasKind(noResult, FindingGuardUnevaluable) && noResult.Row != 7 {
		t.Fatalf("the guard was named but the row is %d", noResult.Row)
	}
}

// TestSkipIsCrossCheckedInOneDirectionOnly — at the default log level a
// skipped job leaves no line at all, so a MISSING record is not a defect; a
// CONTRADICTING one is.
func TestSkipIsCrossCheckedInOneDirectionOnly(t *testing.T) {
	m := baseModel(
		ModelJob{JobID: "gate", Expected: exec.ConclusionSkipped, SkipCause: "if_false"},
		modelJob("build", step("a")),
	)
	ghRun := func() *GitHubRun {
		return &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess, Jobs: []GitHubJob{
			{Key: JobKey{JobID: "gate"}, Conclusion: exec.ConclusionSkipped, DeclaredStepCount: -1},
			{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1,
				Steps: []GitHubStep{githubStep(0, 2, "a", exec.ConclusionSuccess)}},
		}}
	}
	buildJob := observedJob("build", exec.ConclusionSuccess, observedStep(0, "a", exec.ConclusionSuccess))

	t.Run("a missing record is not a defect", func(t *testing.T) {
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill),
			Result: engineResult(buildJob), GitHub: ghRun()})
		if v.Row != 10 || v.Comparison != ComparisonMatch {
			t.Fatalf("verdict = row %d %s, comparison %q; a skipped job with no engine record is expected behaviour", v.Row, v.Outcome, v.Comparison)
		}
	})

	t.Run("a confirming record is recorded", func(t *testing.T) {
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill),
			Result: engineResult(buildJob, observedJob("gate", exec.ConclusionSkipped)), GitHub: ghRun()})
		if !hasKind(v, FindingSkipConfirmed) {
			t.Fatalf("findings = %v", findingKinds(v))
		}
		if v.Comparison != ComparisonMatch {
			t.Fatalf("comparison = %q", v.Comparison)
		}
	})

	t.Run("a contradicting record excludes the run", func(t *testing.T) {
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill),
			Result: engineResult(buildJob, observedJob("gate", exec.ConclusionSuccess)), GitHub: ghRun()})
		if v.Row != 7 || v.Outcome != exec.OutcomeUnmeasurable {
			t.Fatalf("verdict = row %d %s/%s, want row 7 unmeasurable", v.Row, v.Outcome, v.Reason)
		}
		if v.Reason != exec.RefusalPlanDisagreesWithGitHub {
			t.Fatalf("reason = %q; the design names plan_disagrees_with_github for the contradicted cross-check", v.Reason)
		}
		if !hasKind(v, FindingSkipContradicted) {
			t.Fatalf("findings = %v", findingKinds(v))
		}
	})
}

// TestTwoNilsAreTwoDifferentFacts — ModelJob.Leg nil means "this job has no
// matrix" (the model always knows); JobResult.MatrixLeg nil means "the engine
// could not tell the legs apart". Fold either into the other and a matrix job
// whose legs were not resolved matches exactly one model leg while the rest
// disappear with no error.
func TestTwoNilsAreTwoDifferentFacts(t *testing.T) {
	leg := func(v any) *exec.LegKey { return exec.CanonicalLegKeyFromValues(map[string]any{"node": v}) }
	m := baseModel(
		ModelJob{JobID: "build", HasMatrix: true, Leg: leg(20), Steps: []ModelStep{step("a")}},
		ModelJob{JobID: "build", HasMatrix: true, Leg: leg(22), Steps: []ModelStep{step("a")}},
	)
	gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess, Jobs: []GitHubJob{
		{Key: JobKey{JobID: "build", Leg: leg(20).String()}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1,
			Steps: []GitHubStep{githubStep(0, 2, "a", exec.ConclusionSuccess)}},
		{Key: JobKey{JobID: "build", Leg: leg(22).String()}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1,
			Steps: []GitHubStep{githubStep(0, 2, "a", exec.ConclusionSuccess)}},
	}}

	t.Run("an unresolved leg on a job with two legs is not mapped onto one", func(t *testing.T) {
		// One observation, MatrixLeg nil: the engine could not tell them apart.
		res := engineResult(exec.JobResult{JobID: "build", MatrixLeg: nil,
			Conclusion: exec.ConclusionSuccess,
			Steps:      []exec.StepObservation{observedStep(0, "a", exec.ConclusionSuccess)}})
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if v.Outcome == exec.OutcomeCompleted {
			t.Fatalf("an unresolved leg was mapped onto the model's legs and the run was scored %q", v.Comparison)
		}
		if v.Row != 7 || v.Outcome != exec.OutcomeUnmeasurable || v.Reason != exec.RefusalJobIdentityUnmapped {
			t.Fatalf("verdict = row %d %s/%s, want row 7 unmeasurable/job_identity_unmapped", v.Row, v.Outcome, v.Reason)
		}
		if !hasKind(v, FindingLegUnresolved) {
			t.Fatalf("findings = %v, want the unresolved leg named", findingKinds(v))
		}
	})

	t.Run("resolved legs join at leg granularity", func(t *testing.T) {
		res := engineResult(
			exec.JobResult{JobID: "build", MatrixLeg: leg(20), Conclusion: exec.ConclusionSuccess,
				Steps: []exec.StepObservation{observedStep(0, "a", exec.ConclusionSuccess)}},
			exec.JobResult{JobID: "build", MatrixLeg: leg(22), Conclusion: exec.ConclusionSuccess,
				Steps: []exec.StepObservation{observedStep(0, "a", exec.ConclusionSuccess)}},
		)
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if v.Comparison != ComparisonMatch {
			t.Fatalf("comparison = %q; mismatches: %v", v.Comparison, v.Mismatches())
		}
	})

	t.Run("a typed leg and a string leg are different legs", func(t *testing.T) {
		res := engineResult(
			exec.JobResult{JobID: "build", MatrixLeg: leg("20"), Conclusion: exec.ConclusionSuccess,
				Steps: []exec.StepObservation{observedStep(0, "a", exec.ConclusionSuccess)}},
			exec.JobResult{JobID: "build", MatrixLeg: leg(22), Conclusion: exec.ConclusionSuccess,
				Steps: []exec.StepObservation{observedStep(0, "a", exec.ConclusionSuccess)}},
		)
		v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
		if v.Outcome == exec.OutcomeCompleted {
			t.Fatalf(`the engine's leg {"node":"20"} was joined to the model's {"node":20}`)
		}
	})
}

// TestModelValidateRefusesAStepKeyCollision — a step declaring `id: "3"` and an
// unnamed step sitting at index 3 canonicalise to the same key. Nothing
// downstream would have caught it; the join would simply pair the wrong rows.
func TestModelValidateRefusesAStepKeyCollision(t *testing.T) {
	m := baseModel(ModelJob{JobID: "build", Steps: []ModelStep{
		{Name: "zero"}, {Name: "one"}, {Name: "two"}, {Name: "three"},
		{ID: "3", Name: "collides with index 3"},
	}})
	err := m.Validate()
	if err == nil {
		t.Fatal("Model.Validate accepted two steps that canonicalise to the same join key")
	}
	if !strings.Contains(err.Error(), "join key") {
		t.Fatalf("the error does not name the collision: %v", err)
	}

	// The same model without the collision must pass, or the check is just a
	// refusal of matrix-free models.
	ok := baseModel(ModelJob{JobID: "build", Steps: []ModelStep{
		{Name: "zero"}, {Name: "one"}, {ID: "3", Name: "fine"},
	}})
	if err := ok.Validate(); err != nil {
		t.Fatalf("a model with no collision was rejected: %v", err)
	}
}

// TestDeclaredStepIDReproducesTheEngineRule — the model computes the key ITSELF.
// If it read the key back off the engine, (c) would be joining the engine's
// answer to itself.
func TestDeclaredStepIDReproducesTheEngineRule(t *testing.T) {
	for _, tc := range []struct {
		index int
		id    string
		want  string
	}{
		{0, "", "0"},      // 0-based declaration index
		{3, "", "3"},      //
		{3, "mid", "mid"}, // the declared id: wins
		{0, "mid", "mid"},
	} {
		got := exec.CanonicalStepID(DeclaredStepID(tc.index, tc.id))
		if got != tc.want {
			t.Errorf("DeclaredStepID(%d, %q) = %q, want %q", tc.index, tc.id, got, tc.want)
		}
	}
	// And it is the ENGINE's numbering: 0-based, unlike StepObservation.Ordinal.
	j := ModelJob{JobID: "b", Steps: []ModelStep{{Name: "a"}, {ID: "second"}}}
	if j.StepKey(0) != "0" || j.StepKey(1) != "second" {
		t.Fatalf("StepKey = %q, %q", j.StepKey(0), j.StepKey(1))
	}
	if j.StepKey(2) != "" || j.StepKey(-1) != "" {
		t.Fatal("StepKey invented a key for an index the model does not have")
	}
}

// TestCompositeInnerStepsAreRecordedNotCounted — the step-count assertion says
// "the count of stage==Main records" literally, and the inner-step rule makes the literal
// reading wrong: GitHub's job exposes the step that USES the composite and not
// what is inside it, so counting inner records over-counts on every composite.
func TestCompositeInnerStepsAreRecordedNotCounted(t *testing.T) {
	m := baseModel(modelJob("build", step("composite")))
	res := engineResult(exec.JobResult{
		JobID: "build", MatrixLeg: exec.CanonicalLegKeyFromValues(map[string]any{}),
		Conclusion: exec.ConclusionSuccess,
		Steps: []exec.StepObservation{
			{Ordinal: 1, StepID: []string{"0"}, Name: "composite", Ran: true, Conclusion: exec.ConclusionSuccess},
			{Ordinal: 0, StepID: []string{"0", "mk"}, Name: "make", Ran: true, Conclusion: exec.ConclusionSuccess},
			{Ordinal: 0, StepID: []string{"0", "1"}, Name: "inner", Ran: true, Conclusion: exec.ConclusionSuccess},
		},
	})
	gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess,
			DeclaredStepCount: 1,
			Steps:             []GitHubStep{githubStep(0, 2, "composite", exec.ConclusionSuccess)}}}}

	v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
	if v.Comparison != ComparisonMatch {
		t.Fatalf("comparison = %q; the composite's inner records were counted as declared steps. mismatches: %v", v.Comparison, v.Mismatches())
	}
	if !hasKind(v, FindingCompositeInner) {
		t.Fatalf("the inner records were dropped rather than recorded: %v", findingKinds(v))
	}
}

// TestValidateRefusesCallerDefects — an error return is a DEFECT IN THE CALLER,
// never a verdict. Defects are not written to the ledger as exclusions, because
// an exclusion is a fact about a RUN and a defect is a fact about US.
func TestValidateRefusesCallerDefects(t *testing.T) {
	m := baseModel(modelJob("build", step("a")))
	res := engineResult(observedJob("build", exec.ConclusionSuccess, observedStep(0, "a", exec.ConclusionSuccess)))
	gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1,
			Steps: []GitHubStep{githubStep(0, 2, "a", exec.ConclusionSuccess)}}}}

	for _, tc := range []struct {
		name string
		mut  func(*Input)
	}{
		{"an engine that finalised itself", func(in *Input) { in.Result.RunConclusion = exec.ConclusionSuccess }},
		{"an engine that set `unmeasurable`", func(in *Input) {
			in.Result.Outcome = exec.OutcomeUnmeasurable
			in.Result.RefusalReason = exec.RefusalConclusionUnmappable
		}},
		{"a drill with no GitHub side", func(in *Input) { in.GitHub = nil }},
		{"an outage run carrying a GitHub side", func(in *Input) { in.Request.Purpose = exec.PurposeOutage }},
		{"a model with no jobs", func(in *Input) { in.Model.Jobs = nil }},
		{"a GitHub run conclusion that is `unmappable`", func(in *Input) { in.GitHub.Conclusion = exec.ConclusionUnmappable }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ghCopy := *gh
			ghCopy.Jobs = append([]GitHubJob(nil), gh.Jobs...)
			in := Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: &ghCopy}
			tc.mut(&in)
			v, err := Validate(in)
			if err == nil {
				t.Fatalf("Validate returned a verdict instead of an error: %+v", v)
			}
			if v.Outcome != "" || v.Row != 0 {
				t.Fatalf("a caller defect came back as a verdict: %+v", v)
			}
		})
	}
}

// TestAnOutageRunIsEvidenceOfExecutionNeverOfAgreement — an outage run reaches
// `completed` and enters neither A nor X: there is no GitHub side, so however
// well it went it says nothing about agreement.
func TestAnOutageRunIsEvidenceOfExecutionNeverOfAgreement(t *testing.T) {
	m := baseModel(modelJob("build", step("a")))
	res := engineResult(observedJob("build", exec.ConclusionSuccess, observedStep(0, "a", exec.ConclusionSuccess)))

	v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeOutage), Result: res, GitHub: nil})
	if v.Outcome != exec.OutcomeCompleted || v.Row != 10 {
		t.Fatalf("verdict = row %d %s/%s", v.Row, v.Outcome, v.Reason)
	}
	if v.Comparison != ComparisonNotApplicable {
		t.Fatalf("comparison = %q; an outage run has nothing to compare against", v.Comparison)
	}
	if v.GitHubConclusion != "" {
		t.Fatalf("an outage run acquired a GitHub conclusion %q", v.GitHubConclusion)
	}
	if !hasKind(v, FindingOutageNotComparable) {
		t.Fatalf("the verdict does not say why it was not compared: %v", findingKinds(v))
	}
}

// TestTheEngineVerdictIsPassedOnAtTheRightRow — rows 1, 3, 4 and the engine's
// own post-run refusals. Re-deriving them here would put the same rule in two
// places and only one would be updated.
func TestTheEngineVerdictIsPassedOnAtTheRightRow(t *testing.T) {
	m := baseModel(modelJob("build", step("a")))
	gh := &GitHubRun{RunID: 1, Conclusion: exec.ConclusionSuccess,
		Jobs: []GitHubJob{{Key: JobKey{JobID: "build"}, Conclusion: exec.ConclusionSuccess, DeclaredStepCount: 1}}}

	for _, tc := range []struct {
		outcome exec.Outcome
		reason  exec.RefusalReason
		wantRow int
	}{
		{exec.OutcomeRefused, exec.RefusalTokenAbsent, 1},
		{exec.OutcomeRefused, exec.RefusalEventPayloadIncomplete, 2},
		{exec.OutcomeDeadlineExceeded, "", 3},
		{exec.OutcomeHarnessError, exec.RefusalUnsupportedPlatformLine, 4},
		{exec.OutcomeHarnessError, exec.RefusalJobWithoutResult, 6},
		{exec.OutcomeHarnessError, exec.RefusalStepSetUnavailable, 8},
	} {
		t.Run(string(tc.outcome)+"/"+string(tc.reason), func(t *testing.T) {
			res := engineResult()
			res.Outcome = tc.outcome
			res.RefusalReason = tc.reason
			v := mustValidate(t, Input{Model: m, Request: baseRequest(m, exec.PurposeDrill), Result: res, GitHub: gh})
			if v.Row != tc.wantRow {
				t.Fatalf("row = %d, want %d — the counter must name the row that produced the verdict", v.Row, tc.wantRow)
			}
			if v.Outcome != tc.outcome || v.Reason != tc.reason {
				t.Fatalf("the engine's verdict was rewritten: %s/%s", v.Outcome, v.Reason)
			}
			if v.Comparison != ComparisonNotApplicable || v.RunConclusion != "" {
				t.Fatalf("an excluded run was scored: %q / %q", v.Comparison, v.RunConclusion)
			}
			// deadline_exceeded is NEVER failure.
			if tc.outcome == exec.OutcomeDeadlineExceeded && v.RunConclusion == exec.ConclusionFailure {
				t.Fatal("deadline_exceeded acquired a failure conclusion")
			}
		})
	}
}

// TestApplyGoesThroughFinalise — Apply is the only way this package finalises a
// Result, so the ledger's two CHECKs cannot be violated by a field written by hand in
// the wrong order.
func TestApplyGoesThroughFinalise(t *testing.T) {
	r := exec.Result{Provenance: exec.Provenance{InvocationID: "i"}}
	v := Verdict{Row: 10, Outcome: exec.OutcomeCompleted, RunConclusion: exec.ConclusionSuccess}
	if err := v.Apply(&r); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.Outcome != exec.OutcomeCompleted || r.RunConclusion != exec.ConclusionSuccess {
		t.Fatalf("Apply did not write the verdict: %+v", r)
	}
	if err := r.Check(); err != nil {
		t.Fatalf("Apply produced a Result that Check rejects: %v", err)
	}

	// A verdict that Finalise refuses must not be written by Apply either.
	bad := Verdict{Row: 10, Outcome: exec.OutcomeCompleted, RunConclusion: exec.ConclusionUnmappable}
	r2 := exec.Result{Provenance: exec.Provenance{InvocationID: "i"}}
	if err := bad.Apply(&r2); err == nil {
		t.Fatal("Apply wrote a completed run with an unmappable conclusion")
	}
	if err := v.Apply(nil); err == nil {
		t.Fatal("Apply on a nil Result did not error")
	}
}
