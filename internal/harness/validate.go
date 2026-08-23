package harness

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Input is everything the validator needs. Three sets plus the request
// that produced the run.
type Input struct {
	// Model is set P's source: the workflow model, matrix-expanded. NOT the
	// engine's `--list` output, which reports at job-ID granularity only and
	// would leave the validator blind to every matrix leg.
	Model Model

	// Request is the request the engine was given. Purpose, SupplyMode and
	// EventPayload are read from it, so that no caller can hand the validator a
	// different payload from the one that ran.
	Request exec.Request

	// Result is set A: the engine's observation. Its Outcome is the ENGINE's
	// verdict ("no objection"), not the final one: Result is staged,
	// and only Finalise makes it final.
	Result exec.Result

	// GitHub is set G. It is REQUIRED for a drill and must be nil for an
	// outage run: an outage run has no GitHub side and never will, which is
	// why it is kept out of the denominator entirely.
	GitHub *GitHubRun

	// GitHubEventPayload is the payload GitHub used for the run, for the
	// payload check's post-run field-set assertion. Optional; when nil the assertion is
	// recorded as not made rather than passed.
	GitHubEventPayload []byte
}

// Comparison is what row 10 produced.
type Comparison string

const (
	// ComparisonMatch — the derived run conclusion equals GitHub's AND no
	// step-level difference was found. Both halves are required: g11 is a run
	// where the conclusions agree and none of the work was done.
	ComparisonMatch Comparison = "match"
	// ComparisonMismatch — a visible disagreement. The SAFE direction.
	ComparisonMismatch Comparison = "mismatch"
	// ComparisonNotApplicable — there was no GitHub side to compare against
	// (an outage run), or the run never reached row 10.
	ComparisonNotApplicable Comparison = "not_applicable"
)

// Verdict is the validator's answer. It is written onto a Result with Apply.
type Verdict struct {
	// Row is the row of Validate's order that produced this verdict, 1..10.
	Row int

	Outcome       exec.Outcome
	Reason        exec.RefusalReason
	RunConclusion exec.Conclusion // the run-level derivation; only on completed

	// GitHubConclusion is the side we compared against; empty for an outage
	// run.
	GitHubConclusion exec.Conclusion

	Comparison Comparison

	Findings []Finding
}

// Mismatches returns the findings that made this run a mismatch.
func (v Verdict) Mismatches() []Finding {
	var out []Finding
	for _, f := range v.Findings {
		if f.Mismatch {
			out = append(out, f)
		}
	}
	return out
}

// Apply writes the verdict onto the engine's Result.
//
// It is the ONLY way this package finalises a Result, and it goes through
// exec.Result.Finalise so the ledger's two CHECKs cannot be violated by a field
// written by hand in the wrong order.
//
// THE STAGED RESULT HAS AN OPEN BOUNDARY AND THIS DOES NOT CLOSE IT: nothing in the
// interface forces a harness to call Finalise at all, and an engine's
// `completed` type-checks as final. What this package does do is refuse to
// VALIDATE a Result that already carries a RunConclusion, so at least the
// reverse mistake — an engine that finalised itself — is loud. Closing the
// other half needs a separate type for the engine's return, which is the executor
// interface's call and not this package's.
func (v Verdict) Apply(r *exec.Result) error {
	if r == nil {
		return fmt.Errorf("harness: Apply on a nil Result")
	}
	return r.Finalise(v.Outcome, v.Reason, v.RunConclusion)
}

// Finding is a diagnostic. It is NOT part of the closed outcome vocabulary and is
// never a substitute for a refusal: it is the same relief valve
// exec.Provenance.Notes is, so that something the validator noticed is
// not lost merely because the closed set cannot spell it. Anything appearing
// here repeatedly is a report to the maintainers, not a fifteenth constant.
type Finding struct {
	Part   string // "a" | "b" | "c" | "d"
	Kind   string
	Job    string
	Step   string
	Detail string

	// Mismatch marks a finding that makes the run a MISMATCH rather than an
	// exclusion. Row 8's two products must not be confused: an unreadable
	// step set is the denominator's problem, a readable and DIFFERENT one is
	// the numerator's.
	Mismatch bool
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString("(" + f.Part + ") " + f.Kind)
	if f.Job != "" {
		b.WriteString(" job=" + f.Job)
	}
	if f.Step != "" {
		b.WriteString(" step=" + f.Step)
	}
	if f.Detail != "" {
		b.WriteString(": " + f.Detail)
	}
	return b.String()
}

// Finding kinds. Diagnostic strings, deliberately not a closed vocabulary.
const (
	FindingPlanJobNotInGitHub    = "plan_job_not_in_github"
	FindingGitHubJobNotInPlan    = "github_job_not_in_plan"
	FindingGitHubRunEmpty        = "github_run_empty"
	FindingObservedJobNotInPlan  = "observed_job_not_in_plan"
	FindingJobWithoutResult      = "job_without_result"
	FindingJobIdentityUnresolved = "job_identity_unresolved"
	FindingLegUnresolved         = "leg_unresolved"
	FindingGuardUnevaluable      = "guard_unevaluable"
	FindingSkipConfirmed         = "skip_confirmed_by_engine"
	FindingSkipContradicted      = "skip_contradicted_by_engine"
	FindingStepSetUnavailable    = "step_set_unavailable"
	FindingStepCount             = "step_count"
	FindingStepCountUnknown      = "step_count_unknown_on_github_side"
	FindingStepMissing           = "step_missing"
	FindingStepExtra             = "step_extra"
	FindingStepName              = "step_name"
	FindingStepConclusion        = "step_conclusion"
	FindingStepUnjoined          = "step_not_joined_to_github"
	FindingCompositeInner        = "composite_inner_step"
	FindingContinueOnError       = "continue_on_error_applied"
	FindingPayloadUnresolved     = "event_payload_reference_unresolved"
	FindingPayloadRunWasted      = "event_payload_checked_after_the_run"
	FindingPayloadFieldSet       = "event_payload_field_set_differs"
	FindingPayloadNotCompared    = "event_payload_field_set_not_compared"
	FindingConclusionUnmappable  = "conclusion_unmappable"
	FindingFailFastUnmeasured    = "fail_fast_behaviour_unmeasured"
	FindingRunConclusionDiffers  = "run_conclusion_differs"
	FindingEngineVerdictPassedOn = "engine_verdict_passed_on"
	FindingOutageNotComparable   = "outage_run_has_no_github_side"
)

type stepConclusion struct {
	job, step string
	value     exec.Conclusion
}

type validation struct {
	in         Input
	obs        *observedJobs
	reconciled []exec.JobResult
	verdict    Verdict
	decided    bool

	stepConclusions []stepConclusion
}

func (v *validation) finding(f Finding) { v.verdict.Findings = append(v.verdict.Findings, f) }

func (v *validation) mismatch(f Finding) {
	f.Mismatch = true
	v.verdict.Findings = append(v.verdict.Findings, f)
}

func (v *validation) fail(row int, outcome exec.Outcome, reason exec.RefusalReason) {
	if v.decided {
		return
	}
	v.decided = true
	v.verdict.Row = row
	v.verdict.Outcome = outcome
	v.verdict.Reason = reason
	v.verdict.RunConclusion = ""
	v.verdict.Comparison = ComparisonNotApplicable
}

// CheckEventPayload is (d), and it is meant to be called BEFORE the engine
// runs — that is the whole point of putting it at row 2.
//
// It returns the references that do not resolve. A non-empty return means the
// run is `refused` / `event_payload_incomplete` and MUST NOT be spent.
func CheckEventPayload(m Model, payload []byte) (References, error) {
	return Unresolved(payload, ScanModel(m))
}

// Validate runs its ten rows in order and returns the verdict.
//
// THE ORDER IS THE DECISION, NOT AN IMPLEMENTATION DETAIL. Validation happens
// BEFORE derivation: without that, a run that executed zero jobs (an
// unsupported platform, no job runs, no result written, exit 0) passes through
// the derivation's "all skipped" or "otherwise success" rule, acquires a
// conclusion, and is scored as a perfect match. That false match is why the
// validator exists.
//
// An error return is a DEFECT IN THE CALLER, never a verdict: a model that is
// not a plan, a Result that could not be stored, an engine that set an outcome
// only the harness may set. Defects are not written to the ledger as
// exclusions, because an exclusion is a fact about a RUN and a defect is a fact
// about US.
func Validate(in Input) (Verdict, error) {
	if err := in.Model.Validate(); err != nil {
		return Verdict{}, err
	}
	if err := in.Request.Validate(); err != nil {
		return Verdict{}, err
	}
	if err := in.Result.Check(); err != nil {
		return Verdict{}, err
	}
	if in.Result.RunConclusion != "" {
		// Run ALWAYS leaves this empty, because the derivation is
		// above the interface and is conditional on this validator having
		// passed — something no engine can know. A Result that arrives with
		// one has been finalised by something that had no standing to.
		return Verdict{}, fmt.Errorf("harness: the engine's Result already carries run conclusion %q — Run must leave it empty", in.Result.RunConclusion)
	}
	if in.Result.Outcome == exec.OutcomeUnmeasurable {
		// Every cause of `unmeasurable` (rows 5, 7, 9) is a comparison
		// the engine cannot make.
		return Verdict{}, fmt.Errorf("harness: the engine returned `unmeasurable`, which is never an engine's to set")
	}
	switch in.Request.Purpose {
	case exec.PurposeDrill:
		if in.GitHub == nil {
			return Verdict{}, fmt.Errorf("harness: a drill has a GitHub side by definition and Input.GitHub is nil")
		}
		if err := in.GitHub.Validate(); err != nil {
			return Verdict{}, err
		}
	case exec.PurposeOutage:
		if in.GitHub != nil {
			return Verdict{}, fmt.Errorf("harness: an outage run has no GitHub side — GitHub never created that run")
		}
	}

	v := &validation{in: in, obs: indexObserved(in.Result.Jobs)}
	v.verdict.Comparison = ComparisonNotApplicable

	// ROWS 1, 3, 4 and the engine's own post-run refusals. The engine has
	// already decided these and its verdict is passed on unchanged; re-deriving
	// them here would put the same rule in two places, and only one of them
	// would be updated.
	if in.Result.Outcome != exec.OutcomeCompleted {
		v.passOnEngineVerdict()
		return v.verdict, nil
	}

	// ROW 2 — (d), and it should already have run BEFORE the engine did.
	if !v.checkEventPayload() {
		return v.verdict, nil
	}
	// ROW 5 — (a) P ≡ G. Skipped for an outage run: there is no G.
	if in.GitHub != nil {
		if !v.checkPlanAgainstGitHub() {
			return v.verdict, nil
		}
	} else {
		v.finding(Finding{Part: "a", Kind: FindingOutageNotComparable,
			Detail: "purpose=outage: rows 5 and 10's comparison do not apply; the run is evidence of EXECUTION, never of AGREEMENT"})
	}
	// ROW 6 — (a) A ⊆ P, and no job without a result.
	if !v.checkObservedAgainstPlan() {
		return v.verdict, nil
	}
	// ROW 7 — (b), and the reconciled job set the derivation runs over.
	if !v.checkModelMapping() {
		return v.verdict, nil
	}
	// ROW 8 — (c). Produces either an unreadable step set (stop) or mismatches
	// (carry on).
	if !v.checkSteps() {
		return v.verdict, nil
	}
	// ROW 9 — conclusion mapping.
	if !v.checkConclusionMapping() {
		return v.verdict, nil
	}
	// ROW 10 — the run-level derivation and the comparison.
	v.deriveAndCompare()
	return v.verdict, nil
}

// passOnEngineVerdict maps the engine's own outcome onto the row that
// produced it, so the counter names the right row.
func (v *validation) passOnEngineVerdict() {
	res := v.in.Result
	row := 0
	switch res.Outcome {
	case exec.OutcomeRefused:
		row = 1
		if res.RefusalReason == exec.RefusalEventPayloadIncomplete {
			row = 2
		}
	case exec.OutcomeDeadlineExceeded:
		row = 3
	case exec.OutcomeHarnessError:
		switch res.RefusalReason {
		case exec.RefusalUnsupportedPlatformLine:
			row = 4
		case exec.RefusalJobWithoutResult:
			row = 6
		case exec.RefusalStepSetUnavailable:
			row = 8
		default:
			row = 4
		}
	}
	v.finding(Finding{Part: "order", Kind: FindingEngineVerdictPassedOn,
		Detail: "the engine decided this run at row " + itoa(row) + ": " + string(res.Outcome) + "/" + string(res.RefusalReason)})
	v.fail(row, res.Outcome, res.RefusalReason)
}

// checkEventPayload is row 2, run again on this side.
//
// It is CHEAP and deterministic on (model, payload), so re-running it costs
// nothing and catches the one thing the ordering cannot enforce by itself: a
// caller that ran the engine without calling CheckEventPayload first. The run
// is already spent when that happens, and the finding says so — the point of
// row 2's position is that it should not have been.
func (v *validation) checkEventPayload() bool {
	bad, err := Unresolved(v.in.Request.EventPayload, ScanModel(v.in.Model))
	if err != nil {
		v.finding(Finding{Part: "d", Kind: FindingPayloadUnresolved, Detail: err.Error()})
		v.fail(2, exec.OutcomeRefused, exec.RefusalEventPayloadIncomplete)
		return false
	}
	if len(bad) > 0 {
		for _, r := range bad {
			v.finding(Finding{Part: "d", Kind: FindingPayloadUnresolved, Detail: r.String()})
		}
		v.finding(Finding{Part: "d", Kind: FindingPayloadRunWasted,
			Detail: "(d) failed after the engine had already run; it belongs at row 2 so the run is not spent"})
		v.fail(2, exec.OutcomeRefused, exec.RefusalEventPayloadIncomplete)
		return false
	}

	// The payload check's POST-RUN half: the field SETS are compared, not the values —
	// the values differ by construction.
	//
	// THE VERDICT ORDER GIVES THIS NO ROW. It is therefore recorded and does not
	// change the outcome. This is an interpretation, and it is the one
	// place in this file where a true difference is written down without
	// costing anything.
	if v.in.GitHubEventPayload == nil {
		v.finding(Finding{Part: "d", Kind: FindingPayloadNotCompared,
			Detail: "GitHub's own event payload was not supplied; the post-run field-set assertion was NOT made"})
		return true
	}
	ours, err := FieldSet(v.in.Request.EventPayload)
	if err != nil {
		v.finding(Finding{Part: "d", Kind: FindingPayloadNotCompared, Detail: err.Error()})
		return true
	}
	theirs, err := FieldSet(v.in.GitHubEventPayload)
	if err != nil {
		v.finding(Finding{Part: "d", Kind: FindingPayloadNotCompared, Detail: err.Error()})
		return true
	}
	missing, extra := FieldSetDiff(ours, theirs)
	if len(missing) > 0 || len(extra) > 0 {
		v.finding(Finding{Part: "d", Kind: FindingPayloadFieldSet,
			Detail: "fields GitHub carried and we did not: [" + strings.Join(missing, " ") +
				"]; fields we carried and GitHub did not: [" + strings.Join(extra, " ") + "]"})
	}
	return true
}

// checkConclusionMapping is row 9.
//
// Nothing here GUESSES. The engine's mapping onto GitHub's enum has not been
// measured: at step level the vocabulary is closed by source [S] and all three
// values were observed [M]; at job level `skipped` was observed too
// [M]; but cancelled, timed_out, action_required, neutral and stale have
// been observed at NO level. A guessed mapping falls in the one direction that is
// never allowed — a false match.
func (v *validation) checkConclusionMapping() bool {
	ok := true
	for _, j := range v.reconciled {
		if j.Conclusion == exec.ConclusionUnmappable {
			v.finding(Finding{Part: "a", Kind: FindingConclusionUnmappable, Job: j.JobID,
				Detail: "the engine produced a job result with no measured mapping onto GitHub's enum"})
			ok = false
		}
	}
	for _, sc := range v.stepConclusions {
		if sc.value == exec.ConclusionUnmappable {
			v.finding(Finding{Part: "c", Kind: FindingConclusionUnmappable, Job: sc.job, Step: sc.step,
				Detail: "the engine produced a step outcome with no measured mapping onto GitHub's enum"})
			ok = false
		}
	}

	// The fail-fast case, stated as a rule rather than a caution: until the
	// engine's behaviour is measured, runs with `fail-fast: true` and at least one failing leg are
	// written `unmeasurable`. Under fail-fast GitHub CANCELS the remaining legs
	// and the run may be `cancelled`; what the engine produces in that state
	// has NOT been measured, and a gap whose direction is unknown is treated as
	// if it were the false-match direction.
	for _, id := range v.failFastJobsWithAFailingLeg() {
		v.finding(Finding{Part: "a", Kind: FindingFailFastUnmeasured, Job: id,
			Detail: "fail-fast: true with at least one failing leg; the engine's behaviour in that state is unmeasured"})
		ok = false
	}

	if !ok {
		v.fail(9, exec.OutcomeUnmeasurable, exec.RefusalConclusionUnmappable)
	}
	return ok
}

func (v *validation) failFastJobsWithAFailingLeg() []string {
	failing := map[string]bool{}
	for _, j := range v.reconciled {
		if j.Conclusion == exec.ConclusionFailure {
			failing[j.JobID] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, mj := range v.in.Model.Jobs {
		if mj.HasMatrix && mj.FailFast && failing[mj.JobID] && !seen[mj.JobID] {
			seen[mj.JobID] = true
			out = append(out, mj.JobID)
		}
	}
	sort.Strings(out)
	return out
}

// deriveAndCompare is row 10: the run-level derivation and then the comparison.
//
// The derivation runs over the RECONCILED set — the engine's observations plus
// the jobs the model calls `skipped` that the engine legitimately never
// mentioned. That is (b)'s actual product: without it, a workflow whose jobs
// are all `if:`-false reaches the derivation with an empty job list.
func (v *validation) deriveAndCompare() {
	conclusion, err := exec.DeriveRunConclusion(v.reconciled, v.in.Model.ContinueOnError())
	if err != nil {
		// Reaching here means rows 5-9 passed and derivation still refused —
		// an empty job set or an unmappable conclusion. Both are defects in
		// the ordering, not run facts, so this is loud rather than a verdict.
		v.finding(Finding{Part: "a", Kind: FindingConclusionUnmappable, Detail: err.Error()})
		v.fail(9, exec.OutcomeUnmeasurable, exec.RefusalConclusionUnmappable)
		return
	}

	v.decided = true
	v.verdict.Row = 10
	v.verdict.Outcome = exec.OutcomeCompleted
	v.verdict.Reason = ""
	v.verdict.RunConclusion = conclusion

	if v.in.GitHub == nil {
		// An outage run reaches `completed` and enters NEITHER A NOR X: there
		// is no GitHub side, so however well it went it is evidence of
		// execution and never of agreement.
		v.verdict.Comparison = ComparisonNotApplicable
		return
	}

	v.verdict.GitHubConclusion = v.in.GitHub.Conclusion
	if conclusion != v.in.GitHub.Conclusion {
		v.mismatch(Finding{Part: "a", Kind: FindingRunConclusionDiffers,
			Detail: "GitHub concluded " + string(v.in.GitHub.Conclusion) + "; the derivation produced " + string(conclusion)})
	}

	// BOTH HALVES DECIDE. A run whose conclusions agree is NOT a match if a
	// step-level difference was found: that is g11 exactly — both sides
	// `success`, and the engine did none of the work. Scoring it as a match is
	// the failure the validator was rewritten to stop.
	if len(v.verdict.Mismatches()) > 0 {
		v.verdict.Comparison = ComparisonMismatch
		return
	}
	v.verdict.Comparison = ComparisonMatch
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
