package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// The CANDIDATE axis.
//
// It holds every run the selector LOOKED AT, not every run it chose. The
// gate's rules forbid excluding by not-selecting, and before
// these tables nothing anywhere could answer "which runs did you consider".
//
// THE GATE DOES NOT READ ITS NUMBERS FROM HERE (and it was measured). On
// one ledger with a single unclaimed mismatched `online` run, exec_invocation
// says 80.0% and the same query joined through drill_attempt says 100.0%,
// because an INNER JOIN drops the unmatched row in silence. Nothing in this
// file offers A/D/M/X through the candidate axis; DrillRunsWithoutACandidate
// is the cross-check, and its emptiness is what makes the two readings equal.

// ---------------------------------------------------------------------------
// The closed sets
// ---------------------------------------------------------------------------

// CandidateSource is drill_candidate.source: where the run came from.
type CandidateSource string

const (
	// SourceGitHubHistory is GitHub's own run history — the drill's real path.
	SourceGitHubHistory CandidateSource = "github_history"
	// SourceSynthetic is a run the ref prober proposed that GitHub has no
	// counterpart for. The synthetic path is not designed yet, and only this
	// source may carry the prefilter's two dispositions.
	SourceSynthetic CandidateSource = "synthetic"
)

var candidateSources = map[CandidateSource]bool{
	SourceGitHubHistory: true,
	SourceSynthetic:     true,
}

// Valid reports whether s is one of the two.
func (s CandidateSource) Valid() bool { return candidateSources[s] }

// CandidateSources returns the closed set, sorted.
func CandidateSources() []CandidateSource { return sortedSet(candidateSources) }

// Disposition is why a run was or was not selected, and it is closed at SEVEN
// values.
//
// WHAT CARRIES THE RULE IS WHAT THIS SET DOES NOT CONTAIN. There is no member
// meaning "we cannot reproduce it", "the inputs are missing", "act does not
// support this" or "it mismatched last time". Every one of those is a judgement
// about OUR side, and the drill design puts the boundary between candidacy
// and verdict precisely there: a run we cannot replay still becomes a
// candidate, is attempted, is refused by the executor and is paid out of the
// gate's own 5% (X). Dropping it at selection makes the exclusion invisible.
//
// So the absence is the enforcement, and an eighth value is a SCHEMA CHANGE
// and is reviewed as one. The rule cannot be broken by accident; breaking
// it deliberately is visible.
type Disposition string

const (
	// DispositionSelected — chosen; attempted in BOTH supply modes.
	DispositionSelected Disposition = "selected"
	// DispositionDeferred — the day's cap was full. NOT terminal: the run is
	// looked at again tomorrow. The queue order is GitHub's own created_at,
	// oldest first, so deferral cannot become "put the hard ones off".
	DispositionDeferred Disposition = "deferred"
	// DispositionNotConcluded — GitHub has written no conclusion yet.
	DispositionNotConcluded Disposition = "not_concluded"
	// DispositionOutOfScope — outside the drill's universe (shadow != safe, or
	// the repo is not configured). This is item 18's subject, not item 17's.
	DispositionOutOfScope Disposition = "out_of_scope"
	// DispositionAlreadySelected — this GitHub run was selected on an earlier
	// day. The partial unique index is what makes that a fact rather than a
	// convention.
	DispositionAlreadySelected Disposition = "already_selected"
	// DispositionNotCreated — the trigger prefilter says GitHub would not have
	// created this run. SYNTHETIC PATH ONLY: on the history path GitHub did
	// create it, and our filter disagreeing is a finding about the filter.
	DispositionNotCreated Disposition = "not_created"
	// DispositionUndecidable — the prefilter could not decide. Synthetic only,
	// same reason.
	DispositionUndecidable Disposition = "undecidable"
)

var dispositions = map[Disposition]bool{
	DispositionSelected:        true,
	DispositionDeferred:        true,
	DispositionNotConcluded:    true,
	DispositionOutOfScope:      true,
	DispositionAlreadySelected: true,
	DispositionNotCreated:      true,
	DispositionUndecidable:     true,
}

// Valid reports whether d is one of the seven.
func (d Disposition) Valid() bool { return dispositions[d] }

// Dispositions returns the closed set, sorted. A caller that must enumerate it
// derives it from here rather than keeping a second hand-written copy.
func Dispositions() []Disposition { return sortedSet(dispositions) }

// OutOfScopeDetail is what drill_candidate.disposition_detail may say when the
// disposition is DispositionOutOfScope — and ONLY then. For the other five
// non-selected dispositions the column stays free text, because "which cap was
// full" and "GitHub has not judged it yet" are sentences, not classes.
//
// WHY THIS SET EXISTS AT ALL.
// `out_of_scope` carries THREE meanings: `shadow: never`, not configured, and
// GitHub's own machinery. All three landed on the same enum value separated
// only by free text — which is the exact inverse of the disposition set's
// argument, where the anti-exclusion rule is held by what the disposition set cannot SAY. Free text
// makes everything sayable. Until this vocabulary shipped, `out_of_scope` was
// `deferred` before its repair.
//
// The set is closed at THREE and an addition is a schema change, reviewed
// as one — the same discipline Dispositions() is under, for the same
// reason: the value decides whether a run GitHub created leaves the drill's
// universe.
type OutOfScopeDetail string

const (
	// DetailShadowNever — the operator declared `shadow: never` on it.
	DetailShadowNever OutOfScopeDetail = "shadow_never"

	// DetailNotConfigured — the repo or workflow is not configured for the
	// drill. An operator declaration too: nothing about the run says it.
	DetailNotConfigured OutOfScopeDetail = "not_configured"

	// DetailGitHubInternalWorkflow — GitHub's OWN machinery
	// (pages-build-deployment, Dependabot, CodeQL, Copilot). This one is NOT an
	// operator declaration and it is not ours to assert: the schema binds it to
	// GitHubInternalPathPrefix with a biconditional, so it can be written when
	// and only when GitHub's own `path` says so.
	DetailGitHubInternalWorkflow OutOfScopeDetail = "github_internal_workflow"
)

var outOfScopeDetails = map[OutOfScopeDetail]bool{
	DetailShadowNever:            true,
	DetailNotConfigured:          true,
	DetailGitHubInternalWorkflow: true,
}

// Valid reports whether d is one of the three.
func (d OutOfScopeDetail) Valid() bool { return outOfScopeDetails[d] }

// OutOfScopeDetails returns the closed set, sorted. The CHECK literals in 0004
// are derived from here rather than transcribed.
func OutOfScopeDetails() []OutOfScopeDetail { return sortedSet(outOfScopeDetails) }

// GitHubInternalPathPrefix is the EXACT byte string GitHub writes at the front
// of the workflow path on every run of its own machinery:
// `dynamic/<producer>/<name>`, alongside `event = 'dynamic'`. Measured over
// 1,909 such runs in three independently drawn frames, agreeing in both
// directions with zero exceptions (measured 2026-08-23).
//
// THREE THINGS ABOUT IT THAT ARE NOT STYLE:
//
//   - It is a PREFIX MATCH, never a name list. The producer segment is not a
//     closed vocabulary — Copilot review was seen under two different producers
//     — so a list falls silently to the wrong side the moment GitHub adds one.
//   - It is BYTE-EXACT. strings.HasPrefix here and `substr(...) = 'dynamic/'`
//     in the schema, never SQLite's LIKE, which is ASCII-case-INSENSITIVE and
//     would also catch a row spelling `DYNAMIC/`.
//   - It is a POSITIVE match. A rule of the shape "not under .github/workflows/
//     implies not ours" is WRONG: a workflow we wrote that GitHub could not
//     compile comes back with path "BuildFailed" and conclusion
//     `startup_failure`, and that run is OURS.
//
// AND IT IS UNDOCUMENTED. `dynamic` is in no published event enum and the path
// shape is in no REST reference. Universal in the measurement, still OBSERVED
// rather than PROMISED. If GitHub renames it, the schema's biconditional does
// not degrade quietly — it starts aborting inserts, which is the right
// direction for a failure but is a dependency that has to be said out loud.
const GitHubInternalPathPrefix = "dynamic/"

// IsGitHubInternalWorkflowPath reports whether workflowPath names one of
// GitHub's own machine-generated workflows. It is the Go side of 0004's
// drill_candidate_github_internal_is_dynamic_path.
func IsGitHubInternalWorkflowPath(workflowPath string) bool {
	return strings.HasPrefix(workflowPath, GitHubInternalPathPrefix)
}

// prefilterDispositions are the two only a SYNTHETIC candidate may carry. On
// the history path GitHub created the run, so the prefilter has no elimination
// power there at all; the CHECK in 0003 is what closes that channel.
var prefilterDispositions = map[Disposition]bool{
	DispositionNotCreated:  true,
	DispositionUndecidable: true,
}

// scoredDispositions are the two a SYNTHETIC candidate may NOT carry, and the
// reason is the mirror image of the one above.
//
// `synthetic` forces github_run_id NULL; `selected` forces github_conclusion
// NOT NULL. So selecting a synthetic candidate means writing GitHub's verdict
// on a run GitHub has no id for — and that verdict is what the comparison
// scores against, so a fabricated one flows straight into M. A match against a
// number we invented is worth more than a mismatch to whoever finds it later.
//
// `deferred` is barred with it because deferral is a promise to look again, and
// with `selected` barred that promise can never be kept: the row would sit in
// QueuedCandidatesBefore's list forever with nothing able to clear it. The two
// have to be lifted together, so they are barred together.
//
// The synthetic-path design is expected to REPLACE this, not work around it: a synthetic
// candidate that can be scored needs its own run identity and its own answer to
// where a comparable conclusion comes from.
var scoredDispositions = map[Disposition]bool{
	DispositionSelected: true,
	DispositionDeferred: true,
}

// sortedSet returns a set's members in sorted order. Insertion sort: these sets
// have seven members at most, and importing "sort" into this file would be the
// only reason it needed it.
func sortedSet[T ~string](m map[T]bool) []T {
	out := make([]T, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The rows
// ---------------------------------------------------------------------------

// DayZone is one drill_day_zone row: the timezone the day key is drawn in, from
// SinceDay onward.
//
// It has NO foreign key to drill_day and that is the whole design.
// A record that EXPLAINS a key cannot be that key's child: with the FK,
// retention cannot delete the day the zone row names and Retain fails
// permanently; without it the explanation outlives the days it explains.
// Retain never deletes from this table.
//
// Timezone is an IANA name, never an offset. A fixed offset is wrong for half
// the year in any zone that observes DST, and the day it draws shifts by an
// hour at the transition without anything noticing.
type DayZone struct {
	SinceDay string
	Timezone string
}

// Validate checks the same two rules the column CHECKs hold.
func (z DayZone) Validate() error {
	switch {
	case !ValidDayKey(z.SinceDay):
		return fmt.Errorf("shadow: drill_day_zone since_day %q is not YYYY-MM-DD", z.SinceDay)
	case z.Timezone == "":
		return fmt.Errorf("shadow: drill_day_zone %s: timezone is empty — it has no default, because a default draws the day boundary silently", z.SinceDay)
	}
	return nil
}

// Candidate is one drill_candidate row: one run the selector examined.
type Candidate struct {
	CandidateID string
	DrillDay    string
	ExaminedAt  time.Time

	// SelectorRev identifies the code that MADE the decision. The GitHub fields
	// below are the inputs; without this, a past selection cannot be questioned
	// because the thing that judged them is unnamed. Same reason
	// exec_invocation carries the OBSERVED executor version.
	SelectorRev string

	Source CandidateSource

	Repo         string
	WorkflowPath string
	CommitSHA    string // 40 hex
	EventName    string

	// The four GitHub fields are present exactly when Source is
	// SourceGitHubHistory. GitHubCreatedAt is the DEFERRAL ORDER: oldest first,
	// GitHub's own sequence, and it reads nothing from our side.
	GitHubRunID      int64
	GitHubRunAttempt int
	GitHubCreatedAt  time.Time

	// GitHubConclusion is GitHub's own verdict — the set exec.RunConclusions()
	// returns, which is NOT exec.Conclusions() and since 2026-08-23 is not a
	// subset of it either. Each set has one member the other lacks:
	// `unmappable` is OUR word for a conclusion we could not map and GitHub
	// never writes it on a run, while `startup_failure` is GitHub's and no
	// engine produces it. Both are nine.
	//
	// IT WAS EIGHT HERE UNTIL 0004, AND THAT WAS A DEFECT, NOT A CHOICE.
	// `startup_failure` was measured in real run data (1 of
	// 10,897 concluded runs in one frame, 7 of 2,604 in another). The CHECK
	// that omitted it did not skip a row — it ABORTED the INSERT, and
	// the drill may never drop a candidate GitHub created, so a
	// single such run took the day's whole scan with it.
	//
	// Empty means GitHub has not written one, which is exactly
	// DispositionNotConcluded.
	//
	// IT IS NEVER OUR VALUE. drill_candidate_synthetic_never_enters_the_score
	// is what keeps that true: without it, selecting a synthetic candidate
	// would require inventing this field, and it is the field the comparison
	// scores against.
	//
	// It is NOT filtered to `success`. The corpus is "the last N SUCCESSFUL
	// runs", and whether that means `success` or "reached a verdict" is left
	// open on purpose: the narrow
	// one makes the corpus entirely green, and an executor that always answers
	// `success` then matches 100%. This package stores what it is given.
	GitHubConclusion exec.Conclusion

	Disposition Disposition

	// DispositionDetail is REQUIRED for every disposition except
	// DispositionSelected. A reasonless exclusion cannot be written — the day
	// axis's reasonless dropped day, one level down.
	DispositionDetail string
}

// Validate holds the same rules as 0003's CHECKs, before the schema sees them,
// so the caller gets a sentence rather than a constraint name.
func (c Candidate) Validate() error {
	switch {
	case c.CandidateID == "":
		return fmt.Errorf("shadow: CandidateID is empty")
	case !ValidDayKey(c.DrillDay):
		return fmt.Errorf("shadow: candidate %s: DrillDay %q is not YYYY-MM-DD", c.CandidateID, c.DrillDay)
	case c.ExaminedAt.IsZero():
		return fmt.Errorf("shadow: candidate %s: ExaminedAt is zero", c.CandidateID)
	case c.SelectorRev == "":
		return fmt.Errorf("shadow: candidate %s: SelectorRev is empty — a decision with no author cannot be questioned later", c.CandidateID)
	case !c.Source.Valid():
		return fmt.Errorf("shadow: candidate %s: Source %q is not github_history|synthetic", c.CandidateID, c.Source)
	case c.Repo == "":
		return fmt.Errorf("shadow: candidate %s: Repo is empty", c.CandidateID)
	case c.WorkflowPath == "":
		return fmt.Errorf("shadow: candidate %s: WorkflowPath is empty", c.CandidateID)
	case len(c.CommitSHA) != 40:
		return fmt.Errorf("shadow: candidate %s: CommitSHA must be 40 hex characters, got %d", c.CandidateID, len(c.CommitSHA))
	case c.EventName == "":
		return fmt.Errorf("shadow: candidate %s: EventName is empty", c.CandidateID)
	case !c.Disposition.Valid():
		return fmt.Errorf("shadow: candidate %s: Disposition %q is outside the closed set of seven — and note that the set deliberately has no member meaning 'we cannot reproduce it'", c.CandidateID, c.Disposition)
	}

	history := c.Source == SourceGitHubHistory
	switch {
	case history && c.GitHubRunID == 0:
		return fmt.Errorf("shadow: candidate %s: a github_history candidate needs GitHubRunID", c.CandidateID)
	case history && c.GitHubRunAttempt < 1:
		return fmt.Errorf("shadow: candidate %s: a github_history candidate needs GitHubRunAttempt >= 1, got %d", c.CandidateID, c.GitHubRunAttempt)
	case history && c.GitHubCreatedAt.IsZero():
		return fmt.Errorf("shadow: candidate %s: a github_history candidate needs GitHubCreatedAt — it is the deferral order", c.CandidateID)
	case !history && (c.GitHubRunID != 0 || c.GitHubRunAttempt != 0 || !c.GitHubCreatedAt.IsZero()):
		return fmt.Errorf("shadow: candidate %s: a synthetic candidate has no GitHub run, so it may carry no run id, attempt or created time", c.CandidateID)
	}

	if c.GitHubConclusion != "" && !c.GitHubConclusion.IsGitHub() {
		return fmt.Errorf("shadow: candidate %s: GitHubConclusion %q is not one GitHub publishes on a run — the set is exec.RunConclusions(), and `unmappable` is OUR word for a conclusion we could not map",
			c.CandidateID, c.GitHubConclusion)
	}
	switch {
	case c.Disposition == DispositionSelected && c.GitHubConclusion == "":
		return fmt.Errorf("shadow: candidate %s: a selected candidate needs GitHub's conclusion — a run GitHub has not judged has nothing to be compared against", c.CandidateID)
	case c.Disposition == DispositionNotConcluded && c.GitHubConclusion != "":
		return fmt.Errorf("shadow: candidate %s: not_concluded carries conclusion %q — the two say opposite things", c.CandidateID, c.GitHubConclusion)
	case c.Disposition != DispositionSelected && c.DispositionDetail == "":
		return fmt.Errorf("shadow: candidate %s: disposition %q needs a detail — not being selected always carries a sentence", c.CandidateID, c.Disposition)
	case history && prefilterDispositions[c.Disposition]:
		return fmt.Errorf("shadow: candidate %s: disposition %q is the trigger prefilter's, and the prefilter may not eliminate a run GitHub actually created — a disagreement there is a FINDING about the filter", c.CandidateID, c.Disposition)
	case !history && scoredDispositions[c.Disposition]:
		return fmt.Errorf("shadow: candidate %s: a synthetic candidate may not be %q — it has no GitHub run, so reaching the scored path would mean writing GitHub's conclusion for a run GitHub never made, and that value is what M is counted from. The synthetic path has to be designed before it can be scored", c.CandidateID, c.Disposition)
	}

	// The two halves of 0004's mechanism for `out_of_scope`, held here so
	// the caller gets a sentence instead of a constraint name.
	//
	// Order matters for the message, not for the verdict: the vocabulary is
	// asked first, so a row that says `out_of_scope` with free text is told its
	// value is not in the set rather than being told about `dynamic/`.
	internal := IsGitHubInternalWorkflowPath(c.WorkflowPath)
	claimsInternal := c.Disposition == DispositionOutOfScope &&
		c.DispositionDetail == string(DetailGitHubInternalWorkflow)
	switch {
	case c.Disposition == DispositionOutOfScope && !OutOfScopeDetail(c.DispositionDetail).Valid():
		return fmt.Errorf("shadow: candidate %s: out_of_scope detail %q is outside the closed set %v — the value carries THREE meanings (operator's `shadow: never`, not configured, and GitHub's own machinery) and free text separates none of them",
			c.CandidateID, c.DispositionDetail, OutOfScopeDetails())

	case internal && !claimsInternal:
		return fmt.Errorf("shadow: candidate %s: workflow path %q starts with %q, so GitHub says this run is its OWN machinery — it must be recorded as %q/%q, not %q. `out_of_scope` BEATS `not_concluded` here: a run outside the universe is outside it whether or not GitHub has judged it yet",
			c.CandidateID, c.WorkflowPath, GitHubInternalPathPrefix,
			DispositionOutOfScope, DetailGitHubInternalWorkflow, c.Disposition)

	case !internal && claimsInternal:
		return fmt.Errorf("shadow: candidate %s: %q may only be written for a run whose workflow path starts with the exact bytes %q, and this one is %q. The class is named by GITHUB's own datum; asserting it from our side is how a run we simply could not replay would leave the corpus",
			c.CandidateID, DetailGitHubInternalWorkflow, GitHubInternalPathPrefix, c.WorkflowPath)
	}
	return nil
}

// Attempt is one drill_attempt row: a planned run, written BEFORE the process.
//
// InvocationID is empty until the executor row is opened. That gap is the point:
// a run we selected and then failed to invoke must not look the same in
// the ledger as a run we never selected, and exec_invocation cannot record it,
// because there the row is never born at all.
type Attempt struct {
	CandidateID  string
	SupplyMode   exec.SupplyMode
	PlannedAt    time.Time
	InvocationID string
}

// ---------------------------------------------------------------------------
// Writing the day zone
// ---------------------------------------------------------------------------

// RecordDayZone appends one drill_day_zone row.
//
// It is an EXPLICIT act on purpose: changing the timezone may not be a side
// effect of editing configuration, because a zone shift skips a day key (moving
// east) or repeats one (moving west), and in both directions "14 drill-days
// within 21 calendar days" stops being checkable — the unit changed in the
// middle of the window.
//
// The table is append-only, so an existing since_day is refused rather than
// rewritten.
func (db *DB) RecordDayZone(ctx context.Context, z DayZone) error {
	if err := z.Validate(); err != nil {
		return err
	}
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO drill_day_zone (since_day, timezone) VALUES (?, ?)`,
			z.SinceDay, z.Timezone)
		return err
	})
	switch extendedCode(err) {
	case constraintPrimaryKey, constraintUnique:
		return fmt.Errorf("%w: %s", ErrZoneAlreadyRecorded, z.SinceDay)
	}
	return err
}

// LatestDayZone returns the most recent zone row. ok is false when the ledger
// has none, which is the state a drill must refuse to open a day in: without a
// zone row nothing records which timezone the key was drawn in, and the ledger
// cannot tell two zones apart afterwards.
func (db *DB) LatestDayZone(ctx context.Context) (DayZone, bool, error) {
	var z DayZone
	err := db.reader.QueryRowContext(ctx,
		`SELECT since_day, timezone FROM drill_day_zone ORDER BY since_day DESC LIMIT 1`).
		Scan(&z.SinceDay, &z.Timezone)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return DayZone{}, false, nil
	case err != nil:
		return DayZone{}, false, fmt.Errorf("shadow: reading drill_day_zone: %w", err)
	}
	return z, true, nil
}

// DayZones returns every zone row, oldest first. It is how a reader finds out
// which zone a SURVIVING day key was drawn in after the days themselves have
// been purged — the reason the table has no foreign key.
func (db *DB) DayZones(ctx context.Context) ([]DayZone, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT since_day, timezone FROM drill_day_zone ORDER BY since_day`)
	if err != nil {
		return nil, fmt.Errorf("shadow: reading drill_day_zone: %w", err)
	}
	defer rows.Close()
	var out []DayZone
	for rows.Next() {
		var z DayZone
		if err := rows.Scan(&z.SinceDay, &z.Timezone); err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Writing the candidate axis
// ---------------------------------------------------------------------------

// RecordExamination writes EVERY run the selector looked at on one day, in ONE
// transaction.
//
// The batch is one transaction and one day on purpose. A per-row call would let
// a crash mid-scan leave a day holding the runs that were selected and not the
// ones that were rejected — precisely the shape "excluding by not-selecting"
// takes when nobody means it.
//
// A run that was already selected on an earlier day comes back as
// ErrRunAlreadySelected rather than being written: the caller's answer for it is
// a row with DispositionAlreadySelected, not a second selection. The partial
// unique index is what makes that a fact instead of a convention, and it is what
// makes the promotion gate's "20 DISTINCT runs" equal to the denominator query's row
// count.
func (db *DB) RecordExamination(ctx context.Context, day string, examined []Candidate) error {
	if !ValidDayKey(day) {
		return fmt.Errorf("shadow: examination day %q is not YYYY-MM-DD", day)
	}
	// The batch is COPIED before the day is filled in. Writing back into the
	// caller's slice would make a rejected call leave half-mutated rows behind
	// in the caller's own memory, which is a worse trap than the one the empty
	// day is a convenience for.
	rows := make([]Candidate, len(examined))
	copy(rows, examined)

	seen := make(map[string]bool, len(rows))
	for i := range rows {
		c := &rows[i]
		if c.DrillDay == "" {
			c.DrillDay = day
		}
		if c.DrillDay != day {
			return fmt.Errorf("shadow: candidate %s names day %s in an examination of %s — one batch is one day, so a scan cannot straddle a day boundary unnoticed", c.CandidateID, c.DrillDay, day)
		}
		if err := c.Validate(); err != nil {
			return err
		}
		if seen[c.CandidateID] {
			return fmt.Errorf("%w: %s appears twice in one examination", ErrDuplicateCandidate, c.CandidateID)
		}
		seen[c.CandidateID] = true
	}

	var offender string
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		for _, c := range rows {
			offender = c.CandidateID
			// Columns are NAMED, never positional: a positional INSERT turns a
			// new column into a runtime bug instead of a compile error.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO drill_candidate (
					candidate_id, drill_day, examined_at, selector_rev, source,
					repo, workflow_path, commit_sha, event_name,
					github_run_id, github_run_attempt, github_created_at, github_conclusion,
					disposition, disposition_detail
				) VALUES (?,?,?,?,?, ?,?,?,?, ?,?,?,?, ?,?)`,
				c.CandidateID, c.DrillDay, c.ExaminedAt.UnixMilli(), c.SelectorRev, string(c.Source),
				c.Repo, c.WorkflowPath, c.CommitSHA, c.EventName,
				nullInt64(c.GitHubRunID), nullInt(c.GitHubRunAttempt), nullMillis(c.GitHubCreatedAt),
				nullString(string(c.GitHubConclusion)),
				string(c.Disposition), nullString(c.DispositionDetail),
			); err != nil {
				return err
			}
		}
		offender = ""
		return nil
	})
	if err == nil {
		return nil
	}
	// Classified at the statement's own error, not at the transaction's: doing
	// it at the end mapped one constraint onto whichever error the caller
	// happened to be looking for.
	switch extendedCode(err) {
	case constraintPrimaryKey:
		return fmt.Errorf("%w: %s", ErrDuplicateCandidate, offender)
	case constraintUnique:
		// The only UNIQUE besides the primary key is the once-ever selection
		// index.
		return fmt.Errorf("%w: %s names a GitHub run that a previous day already selected — the answer for it is a row with disposition %q, not a second selection",
			ErrRunAlreadySelected, offender, DispositionAlreadySelected)
	case constraintForeignKey:
		return fmt.Errorf("%w: %s (candidate %s)", ErrUnknownDay, day, offender)
	}
	return fmt.Errorf("shadow: drill_candidate %s: %w", offender, err)
}

// PlanAttempts writes BOTH of a selected candidate's attempts, in ONE
// transaction, with invocation_id NULL.
//
// IT TAKES NO MODE, AND THAT IS THE POINT. The drill schedule is
// PAIRED — the same candidate is run in both supply modes, so a difference
// between the modes cannot be confused with a difference between corpora — and
// the second candidate audit exists because nothing held it. A call that could
// plan one mode is a call that will, once. The modes come from
// exec.SupplyModes(), so a third mode arrives here without being transcribed.
//
// The database refuses a candidate that is not `selected`
// (drill_attempt_only_for_selected), which is asked here first so the caller
// gets a sentence.
func (db *DB) PlanAttempts(ctx context.Context, candidateID string, plannedAt time.Time) error {
	if candidateID == "" {
		return errors.New("shadow: PlanAttempts needs a candidate id")
	}
	if plannedAt.IsZero() {
		return fmt.Errorf("shadow: PlanAttempts(%s): plannedAt is zero", candidateID)
	}
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		var disposition string
		err := tx.QueryRowContext(ctx,
			`SELECT disposition FROM drill_candidate WHERE candidate_id = ?`, candidateID).Scan(&disposition)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrUnknownCandidate, candidateID)
		case err != nil:
			return err
		case Disposition(disposition) != DispositionSelected:
			return fmt.Errorf("%w: %s is %q", ErrCandidateNotSelected, candidateID, disposition)
		}
		for _, mode := range exec.SupplyModes() {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO drill_attempt (candidate_id, supply_mode, planned_at, invocation_id)
				VALUES (?,?,?,NULL)`,
				candidateID, string(mode), plannedAt.UnixMilli(),
			); err != nil {
				if extendedCode(err) == constraintPrimaryKey {
					return fmt.Errorf("%w: %s/%s", ErrAttemptAlreadyPlanned, candidateID, mode)
				}
				return fmt.Errorf("shadow: drill_attempt %s/%s: %w", candidateID, mode, err)
			}
		}
		return nil
	})
}

// LinkInvocation points a planned attempt at the invocation row that was opened
// for it. It is a CAS on invocation_id IS NULL, so a lost race comes back as
// ErrAttemptAlreadyLinked rather than re-pointing the attempt.
//
// It also refuses an invocation that CONTRADICTS the attempt — a different
// supply mode, a different day, or a purpose other than `drill`. That is
// the fourth candidate audit held at the write instead of found
// afterwards. The audit is kept regardless: it is what still speaks when the
// writer is not this package, which is the same division of labour
// exec_invocation_no_reverdict has with the CAS in CloseInvocation.
func (db *DB) LinkInvocation(ctx context.Context, candidateID string, mode exec.SupplyMode, invocationID string) error {
	if candidateID == "" || invocationID == "" {
		return errors.New("shadow: LinkInvocation needs a candidate id and an invocation id")
	}
	if !mode.Valid() {
		return fmt.Errorf("shadow: LinkInvocation(%s): supply mode %q is not online|offline", candidateID, mode)
	}
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		var invMode, invDay, purpose, candDay string
		err := tx.QueryRowContext(ctx, `
			SELECT e.supply_mode, e.drill_day, e.purpose, c.drill_day
			FROM exec_invocation e, drill_candidate c
			WHERE e.invocation_id = ? AND c.candidate_id = ?`,
			invocationID, candidateID).Scan(&invMode, &invDay, &purpose, &candDay)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: no invocation %s or no candidate %s", ErrUnknownInvocation, invocationID, candidateID)
		case err != nil:
			return err
		case exec.SupplyMode(invMode) != mode:
			return fmt.Errorf("%w: attempt %s/%s points at an invocation in mode %q", ErrAttemptContradiction, candidateID, mode, invMode)
		case invDay != candDay:
			return fmt.Errorf("%w: candidate %s is on day %s and invocation %s is on day %s", ErrAttemptContradiction, candidateID, candDay, invocationID, invDay)
		case exec.Purpose(purpose) != exec.PurposeDrill:
			return fmt.Errorf("%w: invocation %s has purpose %q, so it is not the drill's work", ErrAttemptContradiction, invocationID, purpose)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE drill_attempt SET invocation_id = ?
			 WHERE candidate_id = ? AND supply_mode = ? AND invocation_id IS NULL`,
			invocationID, candidateID, string(mode))
		if err != nil {
			if extendedCode(err) == constraintUnique {
				return fmt.Errorf("%w: invocation %s is already claimed by another attempt", ErrAttemptAlreadyLinked, invocationID)
			}
			return fmt.Errorf("shadow: linking %s/%s: %w", candidateID, mode, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		// The CAS matched nothing. Which of the two it was matters to whoever
		// reads the log, so it is asked rather than guessed.
		var existing sql.NullString
		err = tx.QueryRowContext(ctx,
			`SELECT invocation_id FROM drill_attempt WHERE candidate_id = ? AND supply_mode = ?`,
			candidateID, string(mode)).Scan(&existing)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s/%s was never planned", ErrUnknownAttempt, candidateID, mode)
		case err != nil:
			return err
		}
		return fmt.Errorf("%w: %s/%s already points at %s", ErrAttemptAlreadyLinked, candidateID, mode, existing.String)
	})
}

// ---------------------------------------------------------------------------
// Reading it back
// ---------------------------------------------------------------------------

// Candidates returns one day's examinations, in GitHub's own created order —
// the same order deferral uses, so a reader sees the queue the selector saw.
// Synthetic rows have no created time and sort last.
func (db *DB) Candidates(ctx context.Context, day string) ([]Candidate, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT candidate_id, drill_day, examined_at, selector_rev, source,
		       repo, workflow_path, commit_sha, event_name,
		       github_run_id, github_run_attempt, github_created_at, github_conclusion,
		       disposition, disposition_detail
		FROM drill_candidate WHERE drill_day = ?
		ORDER BY github_created_at IS NULL, github_created_at, candidate_id`, day)
	if err != nil {
		return nil, fmt.Errorf("shadow: reading drill_candidate: %w", err)
	}
	return scanCandidates(rows)
}

func scanCandidates(rows *sql.Rows) ([]Candidate, error) {
	defer rows.Close()
	var out []Candidate
	for rows.Next() {
		var c Candidate
		var examined int64
		var runID, attempt, created sql.NullInt64
		var conclusion, detail sql.NullString
		if err := rows.Scan(&c.CandidateID, &c.DrillDay, &examined, &c.SelectorRev, &c.Source,
			&c.Repo, &c.WorkflowPath, &c.CommitSHA, &c.EventName,
			&runID, &attempt, &created, &conclusion,
			&c.Disposition, &detail); err != nil {
			return nil, err
		}
		c.ExaminedAt = time.UnixMilli(examined)
		c.GitHubRunID = runID.Int64
		c.GitHubRunAttempt = int(attempt.Int64)
		if created.Valid {
			c.GitHubCreatedAt = time.UnixMilli(created.Int64)
		}
		c.GitHubConclusion = exec.Conclusion(conclusion.String)
		c.DispositionDetail = detail.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// Attempts returns one candidate's planned attempts, by mode.
func (db *DB) Attempts(ctx context.Context, candidateID string) ([]Attempt, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT candidate_id, supply_mode, planned_at, invocation_id
		FROM drill_attempt WHERE candidate_id = ? ORDER BY supply_mode`, candidateID)
	if err != nil {
		return nil, fmt.Errorf("shadow: reading drill_attempt: %w", err)
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var planned int64
		var inv sql.NullString
		if err := rows.Scan(&a.CandidateID, &a.SupplyMode, &planned, &inv); err != nil {
			return nil, err
		}
		a.PlannedAt = time.UnixMilli(planned)
		a.InvocationID = inv.String
		out = append(out, a)
	}
	return out, rows.Err()
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
