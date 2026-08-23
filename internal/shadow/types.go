package shadow

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// ---------------------------------------------------------------------------
// The day axis
// ---------------------------------------------------------------------------

// DayStatus is drill_day.status. Two values, and the ledger will not hold a
// third: a day either had the executor try, or it did not.
type DayStatus string

const (
	// DayRan means the executor TRIED. A day whose drills all ran and were all
	// excluded is still `ran` — A > 0, D = 0 — because writing it `dropped`
	// would move that day's exclusion cost onto the DAY budget.
	DayRan DayStatus = "ran"
	// DayDropped means no drill was attempted. It costs one of the window's
	// seven droppable days and it always carries a reason.
	DayDropped DayStatus = "dropped"
)

// Valid reports whether s is one of the two.
func (s DayStatus) Valid() bool { return s == DayRan || s == DayDropped }

// DropReason is the closed set of reasons a day is dropped. All four drop the
// day and all four come out of the same 7-day budget; they are kept apart
// because "GitHub was gone" and "nobody pushed" are different facts about the
// product, and the second is not evidence of resilience.
type DropReason string

const (
	// DropGitHubUnreachable — the event the product exists for.
	DropGitHubUnreachable DropReason = "github_unreachable"
	// DropExecutorUnavailable — the executor or host side.
	DropExecutorUnavailable DropReason = "executor_unavailable"
	// DropNoCandidateRun — GitHub is up, there was simply no candidate.
	DropNoCandidateRun DropReason = "no_candidate_run"
	// DropOperatorSuspended — the drill was stopped on purpose.
	DropOperatorSuspended DropReason = "operator_suspended"
)

var dropReasons = map[DropReason]bool{
	DropGitHubUnreachable:   true,
	DropExecutorUnavailable: true,
	DropNoCandidateRun:      true,
	DropOperatorSuspended:   true,
}

// Valid reports whether r is one of the four.
func (r DropReason) Valid() bool { return dropReasons[r] }

// DropReasons returns the closed set, sorted. A caller that must enumerate it
// derives it from here rather than keeping a second hand-written copy.
func DropReasons() []DropReason { return sortedSet(dropReasons) }

// dayPattern mirrors the column's GLOB. It is checked in Go as well so the
// caller gets a sentence instead of a CHECK failure; the column is what
// actually holds the line.
var dayPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ValidDayKey reports whether day is a well-formed 'YYYY-MM-DD' key.
//
// It says NOTHING about which timezone drew it. That key is the drill design's
// and is not given here; until it is, the ledger cannot
// tell whether two day rows were drawn in the same zone.
func ValidDayKey(day string) bool { return dayPattern.MatchString(day) }

// Day is one drill_day row.
//
// It holds no counts. A, D and X are derivable per day by grouping
// exec_invocation on drill_day, and a maintained tally is a second copy. The
// one thing that is NOT derivable is why an absence is empty, and that is
// exactly and only what Reason holds.
type Day struct {
	Day    string // 'YYYY-MM-DD' in the drill's configured timezone
	Status DayStatus
	Reason DropReason // required iff Status is DayDropped, forbidden otherwise
}

// Validate enforces the same two rules as the column CHECKs: a reasonless
// dropped day and a reasoned ran day are both rejected.
func (d Day) Validate() error {
	switch {
	case !ValidDayKey(d.Day):
		return fmt.Errorf("shadow: drill day %q is not YYYY-MM-DD", d.Day)
	case !d.Status.Valid():
		return fmt.Errorf("shadow: drill day %s: status %q is not ran|dropped", d.Day, d.Status)
	case d.Status == DayDropped && !d.Reason.Valid():
		return fmt.Errorf("shadow: drill day %s: a dropped day needs a reason from the closed set, got %q", d.Day, d.Reason)
	case d.Status == DayRan && d.Reason != "":
		return fmt.Errorf("shadow: drill day %s: a ran day must not carry drop reason %q", d.Day, d.Reason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The run axis
// ---------------------------------------------------------------------------

// JobSource is exec_job_result.source: the validator's distinction between what the
// engine observed and what the model supplied because the engine left no trace.
type JobSource string

const (
	// SourceExecutor — the engine emitted a result for this job.
	SourceExecutor JobSource = "executor"
	// SourceModel — the engine left NO trace and the conclusion came from the
	// model (an `if:`-false or `needs:`-blocked job). It is not a default: a
	// job whose source is unknown must not be recorded at all.
	SourceModel JobSource = "model"
)

// Valid reports whether s is one of the two.
func (s JobSource) Valid() bool { return s == SourceExecutor || s == SourceModel }

// Image is one exec_image row: one runs-on label's verified ground.
// Every field is an ASSERTION made BEFORE the run, not a value read after it.
type Image struct {
	RunsOnLabel string
	LocalTag    string

	// SourceDigest is the CONFIGURED SOURCE digest — the value identity was
	// asserted AGAINST — and NOT the digest recomputed locally.
	//
	// Under `create_commit` the two already differ, because `docker commit`
	// recomputes `Id` and `RepoDigests`. Writing the local value into the
	// column would turn the record from "what we ran" into "what we produced",
	// the opposite of the executor's duty to record what it ran. Since 0002 the COLUMN says so too — it was renamed
	// `digest` -> `source_digest` — because a name that cannot be read invites
	// the wrong value silently, and under `create_commit` both values exist at
	// the same moment.
	SourceDigest string

	Architecture   string // may not be empty
	ConfigNonEmpty bool
	PathPresent    bool
}

// LocalRepository is one --local-repository style mapping and the revision
// it resolved to BEFORE the run.
//
// A MISSING mirror fails loudly; a STALE one produces a clean success. The
// engine writes the matched path at debug level and the resolved revision
// nowhere, so if it is not written here it exists nowhere. There is no NULL
// SHA: an unreadable mirror is a refusal, not a row.
type LocalRepository struct {
	URL         string
	LocalPath   string
	ResolvedSHA string // 40 hex
}

// Step is one exec_step_observation row: one DECLARED step, or one inner step
// of a composite action.
type Step struct {
	// Ordinal is the declared step order, 1-based. It is NOT the identity and
	// NOT unique — see StepID.
	//
	// 0 means "the engine's channel carried no declared index for this record",
	// which is the case for every `id:`-bearing step and every composite inner
	// step. It is recorded as 0 rather than reconstructed from emission order
	// on purpose: reconstructing shifts every later ordinal by one, silently,
	// the moment a declared step emits nothing — and whether such a step can
	// exist is still an open question.
	Ordinal int

	// StepID is the engine's own step identity, as emitted. len > 1 means an
	// INNER step of a composite action: ["g","mk"] lives inside ["g"].
	//
	// THIS IS THE KEY. The original DDL keyed the table on Ordinal, which cannot hold
	// `g` / `g.mk` / `g.1` — three records with no declared order between them.
	// It may not be empty: a step with no identity cannot be joined against the
	// model at all, and that run is harness_error / step_set_unavailable rather
	// than a row here.
	StepID []string

	Name string // the workflow's `name:`; empty when not declared
	Ran  bool

	// Conclusion is the ENGINE's outcome, not GitHub's conclusion. Empty means
	// the channel carried none. `continue-on-error` is applied from the MODEL
	// by the validator, before comparison — never here.
	Conclusion exec.Conclusion
}

// CanonicalStepID is the dot-joined form stored in exec_step_observation.step_id
// (the step-identity rule).
func (s Step) CanonicalStepID() string { return strings.Join(s.StepID, ".") }

// ValidStepConclusion reports whether c may appear on a STEP.
//
// The step column admits a NARROWER set than the run and job columns: it
// leaves out `action_required` and `stale`. That is the schema's choice and
// it is kept, not widened — but it means an engine may legally produce a
// Conclusion that this table cannot hold, so the exclusion is stated once here
// rather than discovered as a CHECK failure at INSERT.
func ValidStepConclusion(c exec.Conclusion) bool {
	return c.Valid() && c != exec.ConclusionActionRequired && c != exec.ConclusionStale
}

// Job is one (job, matrix leg) observation plus its steps.
type Job struct {
	JobID string

	// Leg is nil when the engine could NOT TELL the legs apart. nil is a
	// measurement outcome, not a default (act stores matrix results per job ID); for act, 1 is expected and 0
	// is an EVENT. A job with no matrix is a non-nil LegKey whose canonical
	// form is "" — a different fact, recorded as leg_resolved = 1.
	Leg *exec.LegKey

	Source     JobSource
	Conclusion exec.Conclusion

	// Steps may be empty, meaning "no step ran". It may NOT stand in for "I do
	// not know": that is harness_error / step_set_unavailable on the
	// invocation, and the caller must not record jobs at all in that case.
	Steps []Step
}

// Residue is one leaked container or volume. This table only COUNTS.
// Cleanup policy belongs to the pool design and is not here.
type Residue struct {
	Kind       string // "container" | "volume"
	Ref        string
	ObservedAt time.Time
}

// ---------------------------------------------------------------------------
// The comparison verdict lives in internal/exec
//
// exec.Comparison was shadow.Comparison until 2026-08-23. It moved because it
// is the VALIDATOR's output, not this ledger's vocabulary: it lived here only
// because its author was boundaried out of internal/exec, and that is the shape
// that produces a second hand copy — a family this project already has five of.
//
// What did NOT change and must not be read as changed: M IS A RECORD, NOT A
// DERIVATION. This ledger holds no GitHub-side conclusion anywhere (the separate
// file was paid for with exactly that), so the gate's numerator is READ
// from exec_invocation.comparison. What is stored is OUR judgement, not
// GitHub's data, which is why recording it does not breach that
// separation.
//
// The column's CHECK literals are still SQL text. They are a transcription of
// one source now rather than a definition, and checkliterals_test.go compares
// them against exec.Comparisons() by reading sqlite_master outward.
//
// The import direction stays ONE-WAY: internal/exec depends on nothing of ours.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Phase 1 — the row that exists BEFORE the process
// ---------------------------------------------------------------------------

// Preflight is what preflight produced, and it is a POINTER on Invocation because
// it can legitimately be absent.
//
// The outcome vocabulary defines preflight refusals — `token_absent`, `normalisation_failed` —
// that stop before these values exist. A run refused because normalisation
// failed has NO text handed to the engine, and the sha of a text that does not
// exist cannot be written. The four columns were NOT NULL until 0002, which
// meant a refused row was required to carry values preflight never produced;
// preflight refusals may not have been recordable at all.
//
// The column CHECK that replaced the NOT NULLs says: if the process was
// invoked, all four are present. Only a `refused` row may stand without them,
// and only RecordPreflightRefusal can write one.
type Preflight struct {
	// ArgvSHA256 is over the invocation's whole normative shape, the working directory
	// INCLUDED: the hash is taken over `cd <WorkDir> && env -i ... act ...`. A
	// hash that leaves the directory out describes two runs in two different
	// repositories identically.
	ArgvSHA256 string

	// WorkflowNormSHA256 is the sha of the normalised text HANDED to the engine, not of
	// the text on disk.
	WorkflowNormSHA256  string
	WorkflowNormApplied bool

	Workdir exec.WorkdirProvenance
}

// Invocation is phase 1: the invocation row as it exists BEFORE the process is
// invoked, with no verdict on it.
//
// WHY THE ROW COMES FIRST. A crashed process cannot emit its own counter. If
// the record were written after the run, a harness that died mid-run would
// leave NOTHING — and such a run was ATTEMPTED, so it belongs in A and must be
// paid out of X. Falling out of both budgets in silence is the run axis's copy
// of "the absence of a day row is a recording defect".
//
// THE RESIDUAL WINDOW, NAMED: phase 1 is written after preflight and
// immediately before the process. A harness that crashes DURING PREFLIGHT still
// leaves no row. It is not closed because the row's own columns are partly
// preflight's output, and a row written without them would not say what it ran.
// This is a boundary, not a measurement.
type Invocation struct {
	InvocationID string
	BackendName  string
	Identity     exec.Identity

	StartedAt time.Time

	Repo               string
	WorkflowPath       string
	CommitSHA          string // 40 hex
	EventName          string
	EventPayloadSHA256 string

	// Purpose has NO default. Since the row is written before the process, the
	// sentence "purpose is written before the run" is now STRUCTURAL and not
	// merely a decision: `purpose` is not among the five columns phase 2 may
	// write, so there is no code path that relabels a bad drill an outage run.
	Purpose exec.Purpose

	// DrillDay is which day this run is written to. A DECISION (the timezone),
	// not a derivation, and the day row must already exist.
	DrillDay string

	// SupplyMode is the gate's SCORE AXIS since the gate became mode-homogeneous: A/D/M/X are kept per mode
	// and D >= 20 is satisfied per mode.
	SupplyMode exec.SupplyMode

	// Preflight is nil ONLY on a preflight refusal, and only
	// RecordPreflightRefusal accepts a nil one.
	Preflight *Preflight

	Images            []Image
	LocalRepositories []LocalRepository

	// Notes are free-text findings, chiefly the `.actrc` finding: `act` can
	// inject flags from its own config file, so "the caller can neither add
	// nor remove a flag" is true of the CALLER and false of the HOST. They
	// are produced during argv construction, which is why they are phase 1.
	//
	// They are stored VERBATIM and nothing here redacts them.
	Notes []string
}

// Validate checks everything phase 1 can be checked for.
//
// The closed sets are checked against internal/exec's own Valid methods rather
// than a local copy, so a value the executor can produce and the ledger cannot
// store shows up here with a sentence. The migration's CHECK literals are still
// a hand-written copy of the same sets; a divergence fails LOUDLY at INSERT,
// which is the safe direction, but it is not yet a mechanism.
func (inv Invocation) Validate() error {
	switch {
	case strings.TrimSpace(inv.InvocationID) == "":
		return fmt.Errorf("shadow: InvocationID is empty")
	case strings.TrimSpace(inv.BackendName) == "":
		return fmt.Errorf("shadow: %s: BackendName is empty", inv.InvocationID)
	case strings.TrimSpace(inv.Identity.Kind) == "":
		return fmt.Errorf("shadow: %s: Identity.Kind is empty", inv.InvocationID)
	case strings.TrimSpace(inv.Identity.Version) == "":
		return fmt.Errorf("shadow: %s: Identity.Version is empty — the OBSERVED version, not the pin", inv.InvocationID)
	case inv.StartedAt.IsZero():
		return fmt.Errorf("shadow: %s: StartedAt is zero", inv.InvocationID)
	case strings.TrimSpace(inv.Repo) == "":
		return fmt.Errorf("shadow: %s: Repo is empty", inv.InvocationID)
	case strings.TrimSpace(inv.WorkflowPath) == "":
		return fmt.Errorf("shadow: %s: WorkflowPath is empty", inv.InvocationID)
	case len(inv.CommitSHA) != 40:
		return fmt.Errorf("shadow: %s: CommitSHA must be 40 hex characters, got %d", inv.InvocationID, len(inv.CommitSHA))
	case strings.TrimSpace(inv.EventName) == "":
		return fmt.Errorf("shadow: %s: EventName is empty", inv.InvocationID)
	case strings.TrimSpace(inv.EventPayloadSHA256) == "":
		return fmt.Errorf("shadow: %s: EventPayloadSHA256 is empty (the trace of an input-less replay)", inv.InvocationID)
	case !inv.Purpose.Valid():
		return fmt.Errorf("shadow: %s: Purpose %q is not drill|outage — it has NO default", inv.InvocationID, inv.Purpose)
	case !ValidDayKey(inv.DrillDay):
		return fmt.Errorf("shadow: %s: DrillDay %q is not YYYY-MM-DD", inv.InvocationID, inv.DrillDay)
	case !inv.SupplyMode.Valid():
		return fmt.Errorf("shadow: %s: SupplyMode %q is not online|offline — the gate's score axis cannot be blank", inv.InvocationID, inv.SupplyMode)
	}

	if p := inv.Preflight; p != nil {
		switch {
		case strings.TrimSpace(p.ArgvSHA256) == "":
			return fmt.Errorf("shadow: %s: Preflight.ArgvSHA256 is empty (the working directory is inside it)", inv.InvocationID)
		case strings.TrimSpace(p.WorkflowNormSHA256) == "":
			return fmt.Errorf("shadow: %s: Preflight.WorkflowNormSHA256 is empty", inv.InvocationID)
		}
	}

	for i, img := range inv.Images {
		switch {
		case strings.TrimSpace(img.RunsOnLabel) == "":
			return fmt.Errorf("shadow: %s: Images[%d] has an empty runs-on label", inv.InvocationID, i)
		case strings.TrimSpace(img.SourceDigest) == "":
			return fmt.Errorf("shadow: %s: Images[%d] (%s) has no source digest", inv.InvocationID, i, img.RunsOnLabel)
		case strings.TrimSpace(img.Architecture) == "":
			return fmt.Errorf("shadow: %s: Images[%d] (%s) has an empty architecture", inv.InvocationID, i, img.RunsOnLabel)
		}
	}
	for i, lr := range inv.LocalRepositories {
		switch {
		case strings.TrimSpace(lr.URL) == "":
			return fmt.Errorf("shadow: %s: LocalRepositories[%d] has an empty url", inv.InvocationID, i)
		case len(lr.ResolvedSHA) != 40:
			return fmt.Errorf("shadow: %s: LocalRepositories[%d] (%s) ResolvedSHA must be 40 hex, got %d — there is no NULL SHA", inv.InvocationID, i, lr.URL, len(lr.ResolvedSHA))
		}
	}
	return validateNotes(inv.InvocationID, inv.Notes)
}

func validateNotes(id string, notes []string) error {
	for i, n := range notes {
		if strings.TrimSpace(n) == "" {
			return fmt.Errorf("shadow: %s: Notes[%d] is empty — a note with nothing in it is not a finding", id, i)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase 2 — the verdict
// ---------------------------------------------------------------------------

// Verdict is phase 2: the five columns a closed row may acquire, plus the child
// rows that only exist after the run.
//
// It is written ONCE. A closed row taking a second verdict would be post-hoc
// labelling on the run axis — the same escape hatch `purpose` is forbidden from
// opening — and a trigger, not a convention, refuses it.
type Verdict struct {
	InvocationID string

	// FinishedAt zero means the process never finished. Under a verdict that is
	// the fifth outcome state and it is reached with OutcomeDeadlineExceeded: the
	// wall clock bounded it, so "never finished" lands in a bucket instead of
	// becoming a third one.
	FinishedAt time.Time

	Outcome       exec.Outcome
	RefusalReason exec.RefusalReason
	RunConclusion exec.Conclusion

	// Comparison is REQUIRED to close a completed DRILL run and FORBIDDEN on
	// anything else. An outage run has no side to compare against; an excluded
	// run was never compared. The column CHECK holds both halves, so the gate's
	// numerator cannot quietly empty through an omission.
	Comparison exec.Comparison

	Jobs    []Job
	Residue []Residue

	// Notes are appended after phase 1's, continuing the same seq.
	Notes []string
}

// Validate holds the same rules as the column CHECKs, before the schema sees
// them. The FINAL record is what they bind — Executor.Run never returns a
// completed Result carrying a conclusion; only the harness's Finalise does,
// because Result is staged.
func (v Verdict) Validate() error {
	if strings.TrimSpace(v.InvocationID) == "" {
		return fmt.Errorf("shadow: Verdict.InvocationID is empty")
	}
	if !v.Outcome.Valid() {
		return fmt.Errorf("shadow: %s: Outcome %q is outside the closed outcome set — an EMPTY outcome is phase 1, not a verdict", v.InvocationID, v.Outcome)
	}

	switch v.Outcome {
	case exec.OutcomeCompleted:
		if !v.RunConclusion.IsGitHub() {
			return fmt.Errorf("shadow: %s: completed needs a conclusion GitHub publishes on a run (exec.RunConclusions(), nine since `startup_failure` was measured), got %q — 'unmappable' means unmeasurable, not concluded", v.InvocationID, v.RunConclusion)
		}
		if v.RefusalReason != "" {
			return fmt.Errorf("shadow: %s: completed must not carry refusal reason %q", v.InvocationID, v.RefusalReason)
		}
	case exec.OutcomeDeadlineExceeded:
		if v.RefusalReason != "" {
			return fmt.Errorf("shadow: %s: deadline_exceeded names its own cause and must not also carry %q", v.InvocationID, v.RefusalReason)
		}
		if v.RunConclusion != "" {
			return fmt.Errorf("shadow: %s: deadline_exceeded must not carry conclusion %q — it is NEVER failure", v.InvocationID, v.RunConclusion)
		}
	default:
		if !v.RefusalReason.Valid() {
			return fmt.Errorf("shadow: %s: outcome %q needs a reason from the closed refusal set, got %q", v.InvocationID, v.Outcome, v.RefusalReason)
		}
		if v.RunConclusion != "" {
			return fmt.Errorf("shadow: %s: %q must not carry conclusion %q (only a completed run has a run-level conclusion)", v.InvocationID, v.Outcome, v.RunConclusion)
		}
	}

	if v.Comparison != "" && !v.Comparison.Valid() {
		return fmt.Errorf("shadow: %s: Comparison %q is not matched|mismatched (the vocabulary is closed at two)", v.InvocationID, v.Comparison)
	}
	if v.Comparison != "" && v.Outcome != exec.OutcomeCompleted {
		return fmt.Errorf("shadow: %s: outcome %q may not carry a comparison — an excluded run was never compared", v.InvocationID, v.Outcome)
	}

	for i, j := range v.Jobs {
		if strings.TrimSpace(j.JobID) == "" {
			return fmt.Errorf("shadow: %s: Jobs[%d] has an empty job id", v.InvocationID, i)
		}
		if !j.Source.Valid() {
			return fmt.Errorf("shadow: %s: Jobs[%d] (%s) source %q is not executor|model — there is no default", v.InvocationID, i, j.JobID, j.Source)
		}
		if !j.Conclusion.Valid() {
			return fmt.Errorf("shadow: %s: Jobs[%d] (%s) conclusion %q is outside the vocabulary", v.InvocationID, i, j.JobID, j.Conclusion)
		}
		for k, s := range j.Steps {
			if s.CanonicalStepID() == "" {
				return fmt.Errorf("shadow: %s: Jobs[%d] (%s) Steps[%d] has no StepID — it is the join key and it cannot be empty", v.InvocationID, i, j.JobID, k)
			}
			if s.Ordinal < 0 {
				return fmt.Errorf("shadow: %s: Jobs[%d] (%s) Steps[%d] ordinal %d is negative", v.InvocationID, i, j.JobID, k, s.Ordinal)
			}
			if s.Conclusion != "" && !ValidStepConclusion(s.Conclusion) {
				return fmt.Errorf("shadow: %s: Jobs[%d] (%s) Steps[%d] conclusion %q is outside the STEP vocabulary (neither action_required nor stale is admitted at step level)", v.InvocationID, i, j.JobID, k, s.Conclusion)
			}
		}
	}
	for i, r := range v.Residue {
		if r.Kind != "container" && r.Kind != "volume" {
			return fmt.Errorf("shadow: %s: Residue[%d] kind %q is not container|volume", v.InvocationID, i, r.Kind)
		}
		if strings.TrimSpace(r.Ref) == "" {
			return fmt.Errorf("shadow: %s: Residue[%d] has an empty ref", v.InvocationID, i)
		}
	}
	return validateNotes(v.InvocationID, v.Notes)
}

// PurposeComparisonRequired reports whether a run with this purpose and outcome
// MUST carry a comparison to close.
//
// It is exported because the writer cannot discover the rule from the type
// system: Verdict.Comparison is a string, and an empty one on a completed drill
// is refused by the DATABASE, not by Go. This is the same rule stated once so a
// caller can ask before it gets a CHECK failure.
func PurposeComparisonRequired(p exec.Purpose, o exec.Outcome) bool {
	return p == exec.PurposeDrill && o == exec.OutcomeCompleted
}
