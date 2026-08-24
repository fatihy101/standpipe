package exec

import (
	"context"
	"fmt"
	"time"
)

// Identity goes into the executor's record. A score cannot be read without knowing
// which engine, at which pin, produced it.
type Identity struct {
	Kind         string // engine family, e.g. the backend's `kind`
	Version      string // the version actually OBSERVED, not the configured pin
	VersionPinOK bool   // does the observed version equal the configured pin
}

// Request is one run. Everything in it is decided by the caller BEFORE the
// engine is asked to do anything.
type Request struct {
	Repo         string // "owner/name"
	CommitSHA    string // 40 hex — the SHA being replayed
	WorkflowPath string // ".github/workflows/test.yml", relative to WorkDir
	EventName    string // "push" | "pull_request" | "workflow_dispatch" | ...

	// EventPayload is canonical JSON and MUST NOT be empty. An engine that
	// runs on an empty payload makes every `github.event.*`
	// guard evaluate false: the guarded steps vanish from the log entirely,
	// and both sides conclude success having done none of the work.
	EventPayload []byte

	// Plan is the matrix-EXPANDED model plan. The engine does not run it and
	// must not read it as a source of truth about what happened: the interface
	// carries it for the completeness assertion. The validator's founding rule
	// is that the engine's output is a MEASUREMENT, never a plan source.
	Plan Plan

	// RunsOn is every runs-on label the plan uses — a closed set. An engine
	// whose platform mapping is missing one of them refuses
	// (RefusalPlatformUnmapped) rather than skipping the job, because a missing
	// key produces a SILENT skip and a zero exit.
	RunsOn []string

	// WorkDir is a clean clone at CommitSHA (obligation U-3).
	WorkDir string

	// SupplyMode — see the type. Recorded, and also a score axis: the online and
	// offline measurements diverged on the same workflows, so they are scored
	// separately.
	SupplyMode SupplyMode

	// Purpose — see the type. No default; the caller says.
	Purpose Purpose

	// Deadline is the wall-clock ceiling (obligation U-5). An engine applies it
	// ITSELF. It must be positive: U-5 is a precondition of the
	// gate's arithmetic, not an implementation detail — without a bound, "never
	// finished" is a third bucket that neither enters the denominator nor can
	// be excluded.
	Deadline time.Duration
}

// Validate checks the fields an engine may not be asked to guess about.
//
// It does NOT check event-payload completeness — that needs the
// workflow model and lives in the harness, above this interface.
func (r Request) Validate() error {
	switch {
	case r.Repo == "":
		return fmt.Errorf("exec: Request.Repo is empty")
	case len(r.CommitSHA) != 40:
		return fmt.Errorf("exec: Request.CommitSHA must be 40 hex characters, got %d", len(r.CommitSHA))
	case r.WorkflowPath == "":
		return fmt.Errorf("exec: Request.WorkflowPath is empty")
	case r.EventName == "":
		return fmt.Errorf("exec: Request.EventName is empty")
	case len(r.EventPayload) == 0:
		return fmt.Errorf("exec: Request.EventPayload is empty (an empty payload makes every github.event.* guard false)")
	case r.WorkDir == "":
		return fmt.Errorf("exec: Request.WorkDir is empty")
	case len(r.RunsOn) == 0:
		return fmt.Errorf("exec: Request.RunsOn is empty (every label needs a mapping)")
	case !r.SupplyMode.Valid():
		return fmt.Errorf("exec: Request.SupplyMode %q is not online|offline", r.SupplyMode)
	case !r.Purpose.Valid():
		return fmt.Errorf("exec: Request.Purpose %q is not drill|outage (it has no default)", r.Purpose)
	case r.Deadline <= 0:
		return fmt.Errorf("exec: Request.Deadline must be positive (obligation U-5)")
	}
	return nil
}

// Result is what an engine returns.
//
// WHAT AN ENGINE MAY SET, AND WHAT IT MAY NOT — this is the one place the
// interface's placement and its signatures have to be read together, so it is
// written out here rather than left to be rediscovered:
//
//   - RunConclusion is ALWAYS EMPTY in a Result returned by Executor.Run.
//     The run-level derivation lives in the harness, above the interface,
//     because the roll-up rule is GitHub's semantics and not any engine's; and
//     roll-up rule 1 makes the derivation conditional on the validator having
//     passed, which an engine cannot know. "Only filled when
//     OutcomeCompleted" is an invariant of the FINAL record (the ledger's
//     CHECK), not
//     of what Run hands back.
//   - Outcome as returned by Run is the ENGINE's verdict and may be
//     OutcomeCompleted, meaning "the engine has no objection". That is not the
//     final word: verdict steps 5-9 can still downgrade it to unmeasurable or
//     harness_error. OutcomeUnmeasurable is never an engine's to set — every
//     one of its causes (steps 5, 7, 9) is a comparison the engine cannot make.
//
// Finalise records both halves once the harness is done.
type Result struct {
	Outcome Outcome

	// RefusalReason is MANDATORY when Outcome is refused, harness_error or
	// unmeasurable; EMPTY for completed and deadline_exceeded.
	RefusalReason RefusalReason

	// RunConclusion is filled only by Finalise (see above).
	RunConclusion Conclusion

	Jobs []JobResult

	// Provenance is U-6: a Result may not be returned without it. What was
	// actually run has to be readable afterwards, or an unexplained mismatch
	// cannot be investigated at all.
	Provenance Provenance

	StartedAt, FinishedAt time.Time
}

// JobResult is one (job, matrix leg) observation.
type JobResult struct {
	JobID string

	// MatrixLeg is nil when the engine CANNOT TELL the legs apart. nil is a
	// measurement outcome, not a default: act's matrix job RESULTS bleed across
	// legs even though their identities do not. A leg that exists and
	// is empty — a job with no matrix — is a non-nil LegKey whose Canonical is
	// "", which is a different fact and is recorded differently (the ledger's
	// leg_resolved).
	MatrixLeg *LegKey

	Conclusion Conclusion

	// Steps is NEVER nil. An empty slice means "no step ran". "I do not know"
	// is OutcomeHarnessError + RefusalStepSetUnavailable (U-4) — an empty slice
	// standing in for ignorance is a silent failure: the work never happens and
	// nothing says so.
	Steps []StepObservation
}

// StepObservation is one DECLARED step (or one inner step of a composite).
type StepObservation struct {
	// Ordinal is the DECLARED step index, 1-based. It is NOT the join key —
	// StepID is.
	//
	// 0 means "the engine's channel did not carry a declared index for this
	// record". That happens for a step that declared an `id:` (the engine emits
	// the id instead of the index) and for the inner steps of a composite. It
	// is recorded as 0 rather than reconstructed from emission order on
	// purpose: reconstructing it would shift every ordinal by one, with no
	// error, the moment a declared step emits nothing — and whether such a step
	// can exist is an open question.
	Ordinal int

	// StepID is the engine's own step identity, as emitted. len(StepID) > 1
	// means an INNER step of a composite action: ["g","mk"] is
	// inside ["g"].
	StepID []string

	Name string // the workflow's `name:`; empty when not declared

	Ran bool

	// Conclusion is the ENGINE's outcome, not GitHub's conclusion. `continue-on-error` is applied FROM THE MODEL, by the validator,
	// BEFORE comparison. An engine that applies it here would be reporting a
	// conclusion it did not observe.
	Conclusion Conclusion
}

// Provenance is U-6's record: what was actually run.
//
// It is the executor's own record and is broader than any single ledger table;
// the ledger's exec_invocation / exec_image / exec_local_repository are written from
// it by the storage slice.
type Provenance struct {
	InvocationID string
	BackendName  string
	Identity     Identity

	// ArgvSHA256 is over the invocation's whole normative shape — the process
	// environment as well as the argument vector — because DOCKER_HOST and the
	// narrow PATH change what ran just as much as a flag does.
	//
	// The secret's VALUE is redacted before hashing. Two runs that differ only
	// in the token hash identically: the field records the shape of the
	// invocation, and must not become an offline-checkable digest of a
	// credential.
	ArgvSHA256 string

	// ArgvRedacted is the same rendering the hash was taken over, kept for a later
	// investigation of a mismatch. It never contains the token.
	ArgvRedacted []string

	WorkflowNormSHA256  string // SHA-256 of the text HANDED to the engine
	WorkflowNormApplied bool   // did normalisation change anything
	EventPayloadSHA256  string

	Images            []ImageProvenance
	LocalRepositories []LocalRepositoryProvenance
	Workdir           WorkdirProvenance

	// ExitCode is recorded and is NOT a result (U-4: "an exit code is not a
	// result"). It exists so an investigation can see it, not so anything can
	// branch on it. -1 means the process never produced one.
	ExitCode int

	// Notes are findings that are true, recorded, and have NO member of the
	// closed result vocabulary to name them. They are never a substitute for a refusal — they
	// exist so that something the executor noticed is not lost merely because
	// the vocabulary cannot spell it. Anything appearing here repeatedly is a
	// design finding to raise, not a new constant.
	Notes []string
}

// ImageProvenance is one runs-on label's verified ground (the ledger's
// exec_image). Every field here is an ASSERTION that was made before the run,
// not a value read afterwards.
type ImageProvenance struct {
	RunsOnLabel    string
	LocalTag       string
	Digest         string // the platform-RESOLVED manifest digest
	Architecture   string // non-empty and equal to the expected platform
	ConfigNonEmpty bool
	PathPresent    bool // Config.Env carries a non-empty PATH
}

// LocalRepositoryProvenance is U-8: one --local-repository style mapping and
// the revision it resolved to BEFORE the run.
//
// A MISSING mirror fails loudly. A STALE one produces a clean success — a
// mirror rewound ~25 commits ran a four-year-old action with no warning at all
// (measured). The engine writes the matched PATH at debug level and the resolved
// REVISION nowhere, so if it is not written here it exists nowhere.
type LocalRepositoryProvenance struct {
	URL         string
	LocalPath   string
	ResolvedSHA string // 40 hex; unreadable means the run was refused
}

// WorkdirProvenance is U-3's assertion plus the ledger's two shape fields.
type WorkdirProvenance struct {
	Clean       bool
	HeadSHA     string
	CommitCount int
	// Shallow is *bool because the ledger's column is nullable: "not a git
	// repository we could ask" and "not shallow" are different facts.
	Shallow *bool
}

// Executor is the executor seam.
type Executor interface {
	Identity() Identity

	// Preflight runs NO work; it ASSERTS that it could. If it returns an error
	// the run is a refusal and Run is not called. Every error it returns
	// carries a RefusalReason — see RefusalError and ReasonOf.
	Preflight(ctx context.Context, req Request) error

	// Run executes and returns a structured job and step set (U-4). A non-nil
	// error means the Result is not usable; a usable Result with a non-completed
	// Outcome is returned with a nil error.
	Run(ctx context.Context, req Request) (Result, error)
}

// Finalise records the harness's verdict on a Result the engine returned.
//
// It is the ONLY way OutcomeCompleted and a RunConclusion end up on the same
// Result, so the ledger's two CHECKs cannot be violated by a caller who filled the
// fields by hand in the wrong order. The adapter's silent hazards (a
// zero-valued options struct, an unmapped correlation field) are the same
// shape: a legal-looking value written at a call site.
//
//   - verdict == OutcomeCompleted requires a GitHub-side conclusion and no
//     reason.
//   - any other verdict requires a reason, except OutcomeDeadlineExceeded which
//     forbids one.
func (r *Result) Finalise(verdict Outcome, reason RefusalReason, conclusion Conclusion) error {
	if !verdict.Valid() {
		return fmt.Errorf("exec: outcome %q is outside the closed set", verdict)
	}
	switch verdict {
	case OutcomeCompleted:
		if !conclusion.IsGitHub() {
			return fmt.Errorf("exec: completed needs a conclusion GitHub publishes on a run, got %q — the ledger admits: %s", conclusion, runConclusionList())
		}
		if reason != "" {
			return fmt.Errorf("exec: completed must not carry a refusal reason, got %q", reason)
		}
	case OutcomeDeadlineExceeded:
		if reason != "" {
			return fmt.Errorf("exec: deadline_exceeded carries its cause in its name and must not also carry %q", reason)
		}
		if conclusion != "" {
			return fmt.Errorf("exec: deadline_exceeded must not carry a conclusion, got %q — it is NEVER failure (U-5)", conclusion)
		}
	default:
		if !reason.Valid() {
			return fmt.Errorf("exec: outcome %q needs a reason from the closed set, got %q", verdict, reason)
		}
		if conclusion != "" {
			return fmt.Errorf("exec: %q must not carry a conclusion, got %q (roll-up rule 1)", verdict, conclusion)
		}
	}
	r.Outcome = verdict
	r.RefusalReason = reason
	r.RunConclusion = conclusion
	return nil
}

// Check enforces the invariants the ledger holds with CHECK constraints, so a Result
// that could not be stored cannot be passed around either.
func (r Result) Check() error {
	if !r.Outcome.Valid() {
		return fmt.Errorf("exec: outcome %q is outside the closed set", r.Outcome)
	}
	switch r.Outcome {
	case OutcomeCompleted:
		if r.RefusalReason != "" {
			return fmt.Errorf("exec: completed carries refusal reason %q", r.RefusalReason)
		}
	case OutcomeDeadlineExceeded:
		if r.RefusalReason != "" {
			return fmt.Errorf("exec: deadline_exceeded carries refusal reason %q", r.RefusalReason)
		}
		if r.RunConclusion != "" {
			return fmt.Errorf("exec: deadline_exceeded carries conclusion %q — it is NEVER failure (U-5)", r.RunConclusion)
		}
	default:
		if !r.RefusalReason.Valid() {
			return fmt.Errorf("exec: outcome %q has no reason from the closed set (got %q)", r.Outcome, r.RefusalReason)
		}
	}
	if r.RunConclusion != "" {
		if r.Outcome != OutcomeCompleted {
			return fmt.Errorf("exec: conclusion %q on non-completed outcome %q (roll-up rule 1)", r.RunConclusion, r.Outcome)
		}
		if !r.RunConclusion.IsGitHub() {
			return fmt.Errorf("exec: run conclusion %q is not one GitHub publishes on a run — the ledger admits: %s", r.RunConclusion, runConclusionList())
		}
	}
	if r.Provenance.InvocationID == "" {
		return fmt.Errorf("exec: Provenance is empty — U-6 forbids returning a Result without it")
	}
	for i, j := range r.Jobs {
		if j.Steps == nil {
			return fmt.Errorf("exec: Jobs[%d] (%s) has a nil step slice — U-4 requires a slice or a refusal", i, j.JobID)
		}
		if !j.Conclusion.Valid() {
			return fmt.Errorf("exec: Jobs[%d] (%s) conclusion %q is outside the vocabulary", i, j.JobID, j.Conclusion)
		}
	}
	return nil
}
