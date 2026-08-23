package harness

import (
	"fmt"
	"strconv"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Model is the workflow model, MATRIX-EXPANDED — set P, plus the
// step-level and expression-level detail that parts (b), (c) and (d) need and
// that exec.Plan deliberately does not carry.
//
// The YAML walk that PRODUCES this is not designed yet. This type is that
// walk's output contract, and it is kept to exactly what the validator needs:
//
//   - the plan record (workflow_path, job_id, leg_key), expanded AFTER
//     include/exclude;
//   - the model-derived expected conclusion, and — the part that stops (b)
//     turning into g11 from the inside — whether the `if:` was EVALUABLE at
//     all;
//   - the declared step list, in declaration order, with each step's `id:`,
//     `name:` and `continue-on-error`;
//   - the job-level `continue-on-error` the derivation's failure rule reads, which has no field in any
//     engine's job model and therefore cannot be read from the run.
type Model struct {
	WorkflowPath string
	EventName    string
	Jobs         []ModelJob

	// Expressions is every ${{ }}-bearing text the model found anywhere in the
	// workflow that is NOT a guard: env, with, run, name, matrix expressions.
	// (d) scans these for `github.event.*` and `inputs.*` references
	// (the payload check's first step).
	//
	// Guards are NOT listed here and must not be: ModelJob.If and ModelStep.If
	// are scanned as WHOLE expressions, because GitHub allows `if:` without the
	// ${{ }} wrapper and a scanner that only looked inside braces would miss
	// `if: github.event.repository.private == true` — which is g11 itself.
	Expressions []string
}

// ModelJob is one matrix-expanded leg of the plan.
type ModelJob struct {
	JobID string

	// Leg is nil when the job HAS NO MATRIX. This is a different fact from
	// exec.JobResult.MatrixLeg's nil, which means "the engine could not tell
	// the legs apart" — the model always knows.
	Leg *exec.LegKey

	// HasMatrix and FailFast feed the unmeasured fail-fast case: a failing leg
	// under fail-fast: true is refused until the engine's behaviour is measured. GitHub's default for
	// fail-fast is TRUE, so a model that leaves this false for a matrix job is
	// claiming something it did not read.
	HasMatrix bool
	FailFast  bool

	// ContinueOnError is the derivation's failure-rule input, read FROM THE
	// MODEL.
	ContinueOnError bool

	// If is the raw job-level guard, unwrapped, for diagnostics and for (d)'s
	// reference scan. Empty means no guard.
	If string

	// Expected is the conclusion the MODEL predicts. Empty means "the model
	// expects this job to run and does not predict its outcome". The only
	// other value this design has a rule for is skipped.
	Expected exec.Conclusion

	// SkipCause names why, for the record: "if_false", "needs_blocked". Free
	// text, diagnostic only.
	SkipCause string

	// Unevaluable, when non-empty, says the model COULD NOT decide this job's
	// guard, and why.
	//
	// The `skipped` mapping's limit lives here and it is the one that keeps (b) honest: a job
	// may only be counted `skipped` when its `if:` was evaluable. An `if:`
	// read through an empty `github.event.*` is not "false", it is UNKNOWN —
	// and confirming it as `skipped` from the model would bring the g11 class
	// back from INSIDE the validator.
	Unevaluable string

	Steps []ModelStep
}

// ModelStep is one DECLARED step, in declaration order.
//
// Composite actions are NOT expanded: the model declares the step that uses
// the composite, and the engine's inner records (len(StepID) > 1) are matched
// against the parent, never counted as declared steps of their own.
type ModelStep struct {
	// ID is the declared `id:`. It is half of the step-naming rule and the
	// model reproduces that rule on its own side — see DeclaredStepID.
	ID string

	// Name is the declared `name:`. Empty means none was declared, and in that
	// case the engine's `step` field is NOT compared: the engine renders a
	// name of its own for unnamed steps and comparing it would manufacture
	// mismatches.
	Name string

	// ContinueOnError is the step-level continue-on-error input, applied to the ENGINE's
	// outcome BEFORE comparison.
	ContinueOnError bool

	If string

	// Expected mirrors ModelJob.Expected at step level: skipped when the
	// model evaluated a step-level `if:` to false, empty otherwise.
	Expected exec.Conclusion

	Unevaluable string
}

// JobKey is one matrix-expanded job's identity: (job_id, leg_key), with
// the workflow path implied by the Model.
type JobKey struct {
	JobID string
	Leg   string // exec.LegKey canonical form; "" means "no matrix"
}

func (k JobKey) String() string {
	if k.Leg == "" {
		return k.JobID
	}
	return k.JobID + " " + k.Leg
}

// Key is the model side of the join.
func (j ModelJob) Key() JobKey { return JobKey{JobID: j.JobID, Leg: j.Leg.String()} }

// Skipped reports whether the MODEL predicts this job is skipped.
func (j ModelJob) Skipped() bool { return j.Expected == exec.ConclusionSkipped }

// DeclaredStepID reproduces, ON THE MODEL SIDE, the rule the engine uses to
// name a step: the declared `id:` when one was given, and the
// 0-based declaration index only when one was not.
//
// The model computes this ITSELF rather than reading it back off the engine.
// That is the validator's single sentence at step level: if the key came from the engine,
// (c) would be joining the engine's answer to itself.
//
// index is 0-based, which is the engine's numbering; exec.StepObservation's
// Ordinal is 1-based and is NOT the join key.
func DeclaredStepID(index int, id string) []string {
	if id != "" {
		return []string{id}
	}
	return []string{strconv.Itoa(index)}
}

// StepKey is the canonical join key for the step at declaration index i.
func (j ModelJob) StepKey(i int) string {
	if i < 0 || i >= len(j.Steps) {
		return ""
	}
	return exec.CanonicalStepID(DeclaredStepID(i, j.Steps[i].ID))
}

// Plan projects the model onto the interface's carrier (Request.Plan).
//
// The projection is deliberately LOSSY: the engine gets the leg set and the
// per-job expectations and gets no step list, because the engine must not be
// able to read the model's step identities and echo them back.
func (m Model) Plan() exec.Plan {
	p := exec.Plan{WorkflowPath: m.WorkflowPath, Jobs: make([]exec.PlanJob, 0, len(m.Jobs))}
	for _, j := range m.Jobs {
		p.Jobs = append(p.Jobs, exec.PlanJob{
			JobID:           j.JobID,
			Leg:             j.Leg,
			ContinueOnError: j.ContinueOnError,
			Expected:        j.Expected,
		})
	}
	return p
}

// ContinueOnError is the derivation's continue-on-error map, keyed by JobID.
//
// It is keyed by JobID and NOT by JobKey because continue-on-error is a job-level
// property in GitHub's own model: `continue-on-error` is declared on the job,
// so every leg of a matrix job shares it. A leg-keyed map would silently miss
// every leg whose key the engine could not resolve.
func (m Model) ContinueOnError() map[string]bool {
	out := map[string]bool{}
	for _, j := range m.Jobs {
		if j.ContinueOnError {
			out[j.JobID] = true
		}
	}
	return out
}

// Validate rejects a model that cannot be the left-hand side of an assertion.
//
// An empty job set is an error and not "a plan with nothing in it": a run with
// no jobs is the zero-jobs-executed shape, and the whole point of ordering validation before
// derivation is that such a run must never acquire a conclusion. If P itself is
// empty there is nothing to assert, and asserting nothing always passes.
func (m Model) Validate() error {
	if m.WorkflowPath == "" {
		return fmt.Errorf("harness: Model.WorkflowPath is empty")
	}
	if m.EventName == "" {
		return fmt.Errorf("harness: Model.EventName is empty")
	}
	if len(m.Jobs) == 0 {
		return fmt.Errorf("harness: Model has no jobs — an empty P asserts nothing and always passes")
	}
	seen := map[JobKey]bool{}
	for i, j := range m.Jobs {
		if j.JobID == "" {
			return fmt.Errorf("harness: Model.Jobs[%d] has an empty JobID", i)
		}
		if j.Expected != "" && !j.Expected.Valid() {
			return fmt.Errorf("harness: Model.Jobs[%d] (%s) Expected %q is outside the vocabulary", i, j.JobID, j.Expected)
		}
		if j.Skipped() && j.Unevaluable != "" {
			return fmt.Errorf("harness: Model.Jobs[%d] (%s) is both skipped and unevaluable — the model may not count an unevaluable guard as skipped", i, j.JobID)
		}
		if !j.HasMatrix && j.Leg != nil && j.Leg.Canonical != "" {
			return fmt.Errorf("harness: Model.Jobs[%d] (%s) has no matrix but carries leg %q", i, j.JobID, j.Leg.Canonical)
		}
		k := j.Key()
		if seen[k] {
			return fmt.Errorf("harness: Model has two jobs with key %s — matrix expansion produced a duplicate leg", k)
		}
		seen[k] = true

		steps := map[string]int{}
		for si, s := range j.Steps {
			if s.Expected != "" && !s.Expected.Valid() {
				return fmt.Errorf("harness: Model.Jobs[%d].Steps[%d] Expected %q is outside the vocabulary", i, si, s.Expected)
			}
			key := exec.CanonicalStepID(DeclaredStepID(si, s.ID))
			if prev, dup := steps[key]; dup {
				// Two steps that canonicalise to the same key make the join
				// pair the wrong rows with no error at all. It happens when a
				// step declares `id: "3"` and another sits at index 3.
				return fmt.Errorf("harness: Model.Jobs[%d] (%s) steps %d and %d share join key %q", i, j.JobID, prev, si, key)
			}
			steps[key] = si
		}
	}
	return nil
}
