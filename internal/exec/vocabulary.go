package exec

import (
	"errors"
	"fmt"
	"sort"
)

// Outcome is the five-valued result state. It is a CLOSED set.
//
// The gate reads it together with RefusalReason: `completed` is the
// denominator (D), the other four are exclusions (X) and each one is counted
// separately against the gate's own 5% budget. Collapsing any two of them
// destroys an accounting distinction that the first validator design already
// got wrong once — it scored three ordinary runs as harness errors.
type Outcome string

const (
	// OutcomeCompleted — ran, produced a run-level conclusion and passed the
	// validator. ONLY the harness may set this together with a conclusion;
	// see Result.RunConclusion for what an engine is allowed to return.
	OutcomeCompleted Outcome = "completed"
	// OutcomeRefused — refused WITHOUT running. A precondition did not hold.
	OutcomeRefused Outcome = "refused"
	// OutcomeHarnessError — it ran, but no measurement was made.
	OutcomeHarnessError Outcome = "harness_error"
	// OutcomeUnmeasurable — the run is legitimate but falls in a class the
	// validator cannot reach.
	OutcomeUnmeasurable Outcome = "unmeasurable"
	// OutcomeDeadlineExceeded — the wall-clock limit was exceeded and the
	// process was killed (U-5). NEVER `failure`, and it carries no
	// RefusalReason: it names its own cause, and a cause held in two places is
	// a cause one of which goes stale.
	OutcomeDeadlineExceeded Outcome = "deadline_exceeded"
)

var outcomes = map[Outcome]bool{
	OutcomeCompleted:        true,
	OutcomeRefused:          true,
	OutcomeHarnessError:     true,
	OutcomeUnmeasurable:     true,
	OutcomeDeadlineExceeded: true,
}

// Valid reports whether o is one of the five.
func (o Outcome) Valid() bool { return outcomes[o] }

// Outcomes returns the closed set, sorted. Same contract as RefusalReasons:
// anything that has to ENUMERATE the five — a counter catalogue, a check that a
// schema's CHECK literals still say what this package says — derives them from
// here instead of writing a second copy.
func Outcomes() []Outcome { return sortedMembers(outcomes) }

// RefusalReason is the closed set of reasons. Every one emits a counter; none
// is free text.
//
// deadline_exceeded is deliberately NOT a member (see OutcomeDeadlineExceeded).
type RefusalReason string

const (
	// Preflight — verdict steps 1 and 2.
	RefusalImageUnverified           RefusalReason = "image_unverified"
	RefusalPlatformUnmapped          RefusalReason = "platform_unmapped"
	RefusalExecutorVersionMismatch   RefusalReason = "executor_version_mismatch"
	RefusalTokenAbsent               RefusalReason = "token_absent"
	RefusalWorkdirDirty              RefusalReason = "workdir_dirty"
	RefusalLocalRepositoryUnresolved RefusalReason = "local_repository_unresolved"
	RefusalNormalisationFailed       RefusalReason = "normalisation_failed"
	RefusalEventPayloadIncomplete    RefusalReason = "event_payload_incomplete"

	// After the run — verdict steps 4, 6 and 8.
	RefusalUnsupportedPlatformLine RefusalReason = "unsupported_platform_line"
	RefusalJobWithoutResult        RefusalReason = "job_without_result"
	RefusalStepSetUnavailable      RefusalReason = "step_set_unavailable"

	// Validation — verdict steps 5, 7 and 9. An engine never emits these.
	RefusalPlanDisagreesWithGitHub RefusalReason = "plan_disagrees_with_github"
	RefusalJobIdentityUnmapped     RefusalReason = "job_identity_unmapped"
	RefusalConclusionUnmappable    RefusalReason = "conclusion_unmappable"
)

var refusalReasons = map[RefusalReason]bool{
	RefusalImageUnverified:           true,
	RefusalPlatformUnmapped:          true,
	RefusalExecutorVersionMismatch:   true,
	RefusalTokenAbsent:               true,
	RefusalWorkdirDirty:              true,
	RefusalLocalRepositoryUnresolved: true,
	RefusalNormalisationFailed:       true,
	RefusalEventPayloadIncomplete:    true,
	RefusalUnsupportedPlatformLine:   true,
	RefusalJobWithoutResult:          true,
	RefusalStepSetUnavailable:        true,
	RefusalPlanDisagreesWithGitHub:   true,
	RefusalJobIdentityUnmapped:       true,
	RefusalConclusionUnmappable:      true,
}

// Valid reports whether r is one of the fourteen.
func (r RefusalReason) Valid() bool { return refusalReasons[r] }

// RefusalReasons returns the closed set, sorted, so a caller that has to
// enumerate it (a counter catalogue, a schema check) derives it from here
// rather than keeping a second hand-written copy. `internal/metrics` learned
// what a hand copy costs: its catalogue was a hand copy nothing audited, and
// three transcription slips made the drift audit accuse correct code.
func RefusalReasons() []RefusalReason { return sortedMembers(refusalReasons) }

// sortedMembers is the one implementation behind every enumerator in this file.
// Each closed set holds its membership in a map, so a per-type copy of this
// loop would be four chances to enumerate a set differently from the way its
// own Valid() reads it — the hand-copy shape this file exists to avoid.
func sortedMembers[T ~string](set map[T]bool) []T {
	out := make([]T, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Conclusion is GitHub's `conclusion` vocabulary plus exactly one value this
// design adds: ConclusionUnmappable.
//
// It is NOT one closed set but TWO overlapping ones, which is why the type
// carries two predicates:
//
//   - Valid()    — may appear on a JOB result, i.e. on something an ENGINE
//     produced. `unmappable` is a member, `startup_failure` is not.
//   - IsGitHub() — GitHub may publish it. `startup_failure` is a member,
//     `unmappable` never is.
//
// Until `startup_failure` was measured the two were NESTED — run = job minus
// `unmappable` — and one map of bools could carry both, with IsGitHub written
// as "a member, and not `unmappable`". They are no longer nested: each set has
// a member the other lacks. Membership is therefore held as a SCOPE per value,
// in ONE map, which both predicates and both enumerators read. A second map
// would be the hand-copy shape this file exists to prevent: every closed set
// here can list itself so that a second copy (shadow.db's CHECK literals) can
// be checked instead of trusted.
//
// `in_progress` can NEVER be a member of either. That is a value of GitHub's
// `status` field (queued, in_progress, completed), and confusing the two was
// an early, expensive mistake; the ledger's DDL holds the same line with a
// CHECK.
type Conclusion string

const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionFailure        Conclusion = "failure"
	ConclusionCancelled      Conclusion = "cancelled"
	ConclusionSkipped        Conclusion = "skipped"
	ConclusionTimedOut       Conclusion = "timed_out"
	ConclusionActionRequired Conclusion = "action_required"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionStale          Conclusion = "stale"

	// ConclusionStartupFailure is GitHub's, and it is admitted at RUN LEVEL
	// ONLY: IsGitHub() accepts it, Valid() does not.
	//
	// IT IS A MEASUREMENT, NOT A DESIGN CHOICE [V — 2026-08-23]: 1 of
	// 10,897 concluded runs in the activity-weighted frame and 7 of 2,604 in
	// the uniform frame, on public repos. It was missing here, so the ledger's
	// `run_conclusion` CHECK and the drill candidate's `github_conclusion`
	// CHECK — both derived from RunConclusions() — did not admit it, and a run
	// GitHub really created would have ABORTED THE INSERT. A candidate GitHub
	// created may not be silently dropped, so the
	// failure mode was the whole day's scan aborting, not one row skipped.
	//
	// WHY RUN-LEVEL ONLY, and what that rests on. The measurement is on RUNS.
	// Nothing has observed a JOB carrying it, so job-level membership is not
	// assumed by symmetry; the direction was chosen on cost, and the two
	// directions do not cost the same:
	//
	//   - The run columns (exec_invocation.run_conclusion,
	//     drill_candidate.github_conclusion) hold GITHUB's verdict on a real
	//     run. A value GitHub emits and this set omits aborts an insert. That
	//     is the measured hazard, and admitting it here closes it.
	//   - The job column (exec_job_result.conclusion) holds an ENGINE's result
	//     — ours, never GitHub's. No engine produces `startup_failure`, so
	//     omitting it there cannot abort anything GitHub created.
	//   - Admitting it at job level is NOT free. shadow.ValidStepConclusion is
	//     SUBTRACTIVE (Valid() minus `action_required` and `stale`), so
	//     widening Valid() would silently widen the STEP set too — a schema
	//     narrowing that is shadow's decision to make, not this package's.
	//
	// A GitHub-side job or step that DID carry it is still not rejected: the
	// harness validates GitHub's side with IsGitHub(), not Valid() (see
	// harness.GitHubRun.Validate). So this narrowing removes no GitHub value
	// from any path that reads GitHub. If a job is ever measured carrying it,
	// the change is one scope flag here.
	//
	// IT DOES NOT IMPLY THE `BuildFailed` SENTINEL. A workflow that failed to
	// compile is reported with path "BuildFailed", name "" and a workflow
	// object in state "deleted" — and THAT RUN IS OURS, a file we wrote that
	// GitHub could not compile, not GitHub's own machinery. `BuildFailed`
	// implies `startup_failure`; the converse does NOT hold — it also occurs
	// with a real `.github/workflows/*.yml` path.
	ConclusionStartupFailure Conclusion = "startup_failure"

	// ConclusionUnmappable is this design's own addition. The engine
	// produced a result whose mapping onto GitHub's enum has NOT been
	// measured. An implementation must NEVER guess: a guessed mapping falls
	// exactly in the forbidden direction, a false match. A run carrying it
	// becomes unmeasurable at verdict step 9.
	ConclusionUnmappable Conclusion = "unmappable"
)

// conclusionScope is which of the two sets a value belongs to. One map, read by
// Valid, IsGitHub, Conclusions and RunConclusions alike, so none of the four can
// drift from the other three.
type conclusionScope uint8

const (
	// scopeJob — admitted on a JOB result: what Valid() reads and what
	// exec_job_result.conclusion admits.
	scopeJob conclusionScope = 1 << iota
	// scopeRun — GitHub may publish it on a RUN: what IsGitHub() reads and what
	// exec_invocation.run_conclusion and drill_candidate.github_conclusion
	// admit.
	scopeRun

	// scopeBoth — GitHub's own eight, which are legal at either level.
	scopeBoth = scopeJob | scopeRun
)

// conclusions is the ONE membership map.
//
// `timed_out`, `neutral` and `stale` were NOT observed in the ~13,500 runs the
// 2026-08-23 measurement covered. That is absence of observation and NOT evidence of
// absence — it is not a confirmation that GitHub emits them, and it is not a
// reason to narrow the set either. They stay members; see
// TestUnobservedConclusionsAreNotNarrowed.
var conclusions = map[Conclusion]conclusionScope{
	ConclusionSuccess:        scopeBoth,
	ConclusionFailure:        scopeBoth,
	ConclusionCancelled:      scopeBoth,
	ConclusionSkipped:        scopeBoth,
	ConclusionTimedOut:       scopeBoth,
	ConclusionActionRequired: scopeBoth,
	ConclusionNeutral:        scopeBoth,
	ConclusionStale:          scopeBoth,

	// GitHub's, at run level only — see ConclusionStartupFailure.
	ConclusionStartupFailure: scopeRun,
	// Ours, at job level only: GitHub never writes it on a run.
	ConclusionUnmappable: scopeJob,
}

// Valid reports whether c may appear as a JOB-level conclusion —
// ConclusionUnmappable included, ConclusionStartupFailure excluded.
//
// It is the predicate for a conclusion THIS SIDE produced. A GitHub-side value
// is checked with IsGitHub, which is a different set and not a superset.
func (c Conclusion) Valid() bool { return conclusions[c]&scopeJob != 0 }

// IsGitHub reports whether GitHub may publish c — i.e. whether it may appear as
// a RUN-level conclusion. The ledger's `run_conclusion` CHECK does not admit
// `unmappable`, because a run whose conclusion could not be mapped is
// unmeasurable rather than concluded; it DOES admit `startup_failure`, which
// GitHub emits and no engine produces.
func (c Conclusion) IsGitHub() bool { return conclusions[c]&scopeRun != 0 }

// Conclusions returns the JOB-level set, sorted — exactly what Valid admits.
func Conclusions() []Conclusion { return conclusionsInScope(scopeJob) }

// RunConclusions returns the RUN-level set, sorted — exactly what IsGitHub
// admits, i.e. GitHub's own values.
//
// It is a separate enumerator rather than a caller-side filter of Conclusions
// because the two sets are NOT NESTED: each has a member the other lacks
// (`unmappable` is job-only, `startup_failure` is run-only). A caller filtering
// Conclusions() itself would both keep a value GitHub never writes and miss one
// GitHub does — and the second of those aborts an INSERT.
func RunConclusions() []Conclusion { return conclusionsInScope(scopeRun) }

// conclusionsInScope derives one of the two sets from the shared map, through
// the same sortedMembers every other enumerator in this file uses.
func conclusionsInScope(s conclusionScope) []Conclusion {
	in := make(map[Conclusion]bool, len(conclusions))
	for c, scope := range conclusions {
		if scope&s != 0 {
			in[c] = true
		}
	}
	return sortedMembers(in)
}

// SupplyMode is the outage-rehearsal mode. It is NOT an engine flag: it
// is the question every engine has to answer — "are you allowed to reach
// outside?" — and each one translates it into its own mechanism.
//
// It is also the gate's SCORE AXIS: A/D/M/X are kept per mode and D >= 20 is
// satisfied per mode, because a measurement of the same workflows on the same
// day passed online and failed offline, and a pooled score would be decided by
// the runs that cannot tell the modes apart. Not
// recording it is the same thing as pooling two modes into one denominator.
type SupplyMode string

const (
	SupplyOnline  SupplyMode = "online"
	SupplyOffline SupplyMode = "offline"
)

var supplyModes = map[SupplyMode]bool{
	SupplyOnline:  true,
	SupplyOffline: true,
}

// Valid reports whether m is one of the two.
func (m SupplyMode) Valid() bool { return supplyModes[m] }

// SupplyModes returns the closed set, sorted. Same contract as Outcomes: the
// two modes are CHECK literals in a schema as well as constants here, and a
// set that cannot list itself cannot be compared against its own copy.
func SupplyModes() []SupplyMode { return sortedMembers(supplyModes) }

// Purpose says whether a GitHub side to compare against EXISTS. It is not a
// mode: a drill may run Offline.
//
// It has NO default and is written BEFORE the run. If it could be labelled
// afterwards, a drill that went badly could be declared an outage and escape
// the gate's budget.
type Purpose string

const (
	// PurposeDrill — the gate's candidate. A GitHub-side run exists.
	PurposeDrill Purpose = "drill"
	// PurposeOutage — work actually carried during an outage. There is no
	// GitHub side and there never will be, so it is evidence of EXECUTION and
	// never evidence of AGREEMENT. It enters neither A nor X.
	PurposeOutage Purpose = "outage"
)

var purposes = map[Purpose]bool{
	PurposeDrill:  true,
	PurposeOutage: true,
}

// Valid reports whether p is one of the two.
func (p Purpose) Valid() bool { return purposes[p] }

// Purposes returns the closed set, sorted. Same contract as SupplyModes.
func Purposes() []Purpose { return sortedMembers(purposes) }

// Comparison is the validator's verdict at verdict step 10, and the gate's
// NUMERATOR (M).
//
// It lives HERE, beside Outcome, because it is the VALIDATOR'S OUTPUT and not
// any ledger's vocabulary — the same reason DeriveRunConclusion lives above the
// interface. The harness produces it; an engine never does; a store
// transcribes it into a CHECK and does not define it (otherwise the store would hold
// a second hand copy).
//
// The vocabulary is CLOSED AT TWO because the final verdict step closes it
// at two: "a
// match or a mismatch".
//
// WHAT IT DOES NOT CARRY: a mismatch's EXPLANATION. The gate's third condition
// — zero UNEXPLAINED mismatches over the last 7 drill days — needs a shape that
// does not exist yet and is an open design item. Do NOT
// widen this set to hold it: two-valued closure is what makes the verdict
// storable as a checked column at all, and an `unexplained_mismatch` member
// added in passing would be open design entering a closed set. The explanation
// gets its own carrier when it has a shape.
type Comparison string

const (
	// ComparisonMatched — our run-level conclusion agreed with GitHub's.
	// Summed over a mode's drill runs, this value IS M.
	ComparisonMatched Comparison = "matched"
	// ComparisonMismatched — the two sides disagreed. Whether the
	// disagreement is EXPLAINED is not recorded by this type.
	ComparisonMismatched Comparison = "mismatched"
)

var comparisons = map[Comparison]bool{
	ComparisonMatched:    true,
	ComparisonMismatched: true,
}

// Valid reports whether c is one of the two.
func (c Comparison) Valid() bool { return comparisons[c] }

// Comparisons returns the closed set, sorted. Same contract as Outcomes.
func Comparisons() []Comparison { return sortedMembers(comparisons) }

// RefusalError is how a Preflight failure names its reason.
//
// The interface gives Preflight an `error` return and requires every refusal
// to carry one of the fourteen. Without a typed carrier the harness would have to
// GUESS which reason a preflight error meant, and a guessed reason is a wrong
// counter — a silent failure of the worst kind, because it looks like data. Every
// preflight failure an implementation returns must be one of these.
type RefusalError struct {
	Reason RefusalReason
	Err    error
}

func (e *RefusalError) Error() string {
	if e.Err == nil {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}

func (e *RefusalError) Unwrap() error { return e.Err }

// Refuse builds a RefusalError. It panics on a reason outside the closed set,
// because a reason invented at a call site is precisely what the closed vocabulary forbids and
// a compile-time constant cannot be checked any other way.
func Refuse(reason RefusalReason, err error) *RefusalError {
	if !reason.Valid() {
		panic("exec: refusal reason outside the closed set: " + string(reason))
	}
	return &RefusalError{Reason: reason, Err: err}
}

// ReasonOf extracts the refusal reason from a Preflight error.
//
// ok=false means the error did NOT name a reason. The caller must treat that as
// a defect and surface it, never as a default reason — silently picking one
// would write a counter that is not true.
func ReasonOf(err error) (RefusalReason, bool) {
	var re *RefusalError
	if errors.As(err, &re) && re.Reason.Valid() {
		return re.Reason, true
	}
	return "", false
}
