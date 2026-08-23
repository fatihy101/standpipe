package shadow

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// This file is the ledger's audit surface. Three of the queries must
// come back EMPTY on a sound ledger, and their value is entirely in the fact
// that they FIRE on a broken one: a check that never fires is not a check.
// Each was driven against a deliberately corrupted ledger before it was written
// here.

// queryer is *sql.DB or *sql.Conn. The three audit queries that run in front
// of retention have to run BOTH on the read pool (an operator asking the
// ledger a question) and on the writer connection INSIDE the retention
// transaction (the same call that is about to delete). Anything else is a
// second hand copy of the SQL, and the copy that drifts is the one that stops
// naming the defect.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DrillOnDroppedDay is one row of the first audit query.
type DrillOnDroppedDay struct {
	InvocationID string
	Day          string
	DropReason   DropReason
}

// DrillRunsOnDroppedDays returns drill runs written to a day the ledger says
// was dropped. MUST BE EMPTY: the day axis and the run axis contradict each
// other.
//
// It filters on purpose = 'drill' and that filter is the point. An OUTAGE run
// on a `github_unreachable` day is not a defect — it is the product working,
// and it is the only evidence Phase 1's finishing condition (a workflow runs
// end to end while GitHub is unreachable) has.
func (db *DB) DrillRunsOnDroppedDays(ctx context.Context) ([]DrillOnDroppedDay, error) {
	return drillRunsOnDroppedDays(ctx, db.reader)
}

func drillRunsOnDroppedDays(ctx context.Context, q queryer) ([]DrillOnDroppedDay, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT e.invocation_id, e.drill_day, d.drop_reason
		FROM exec_invocation e
		JOIN drill_day d ON d.day = e.drill_day
		WHERE e.purpose = 'drill' AND d.status = 'dropped'
		ORDER BY e.drill_day, e.invocation_id`)
	if err != nil {
		return nil, fmt.Errorf("shadow: audit 1: %w", err)
	}
	defer rows.Close()
	var out []DrillOnDroppedDay
	for rows.Next() {
		var r DrillOnDroppedDay
		var reason sql.NullString
		if err := rows.Scan(&r.InvocationID, &r.Day, &reason); err != nil {
			return nil, err
		}
		r.DropReason = DropReason(reason.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RanDaysWithoutDrillRuns returns days marked `ran` that carry no drill run.
// MUST BE EMPTY.
//
// This is a SILENT DROPPED DAY: the ledger says the drill ran, and there is no
// work behind it. It is the day axis's copy of the silent-success failure
// class — nothing errored, nothing degraded, the work simply never happened and
// the record says fine.
func (db *DB) RanDaysWithoutDrillRuns(ctx context.Context) ([]string, error) {
	return ranDaysWithoutDrillRuns(ctx, db.reader)
}

func ranDaysWithoutDrillRuns(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT d.day FROM drill_day d
		WHERE d.status = 'ran'
		  AND NOT EXISTS (SELECT 1 FROM exec_invocation e
		                  WHERE e.drill_day = d.day AND e.purpose = 'drill')
		ORDER BY d.day`)
	if err != nil {
		return nil, fmt.Errorf("shadow: audit 2: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Window is the third audit query: the shape of the drill window.
//
// 14 drill-days may fall inside at most 21 calendar days, i.e. at most 7 days
// may be dropped. Exceeding that restarts the count. This type reports the
// numbers; whether the window has been exceeded is the gate's judgement, not
// the ledger's, and NOTHING HERE JUDGES IT.
//
// The 21 did enter the package on 2026-08-23, once, as GateWindowDays, and the
// distinction is worth keeping: retention needs the number to know what it may
// NOT delete (retention may never cut into the gate's window). Refusing to delete inside the window is not the
// same act as ruling the window exceeded, and this query still does neither.
type Window struct {
	DrillDays   int // status = 'ran'
	DroppedDays int // status = 'dropped'
	WindowDays  int // rows — calendar days the ledger covers
}

// Window counts the day axis.
func (db *DB) Window(ctx context.Context) (Window, error) {
	return windowShape(ctx, db.reader)
}

func windowShape(ctx context.Context, q queryer) (Window, error) {
	var w Window
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(status = 'ran'), 0),
		       COALESCE(SUM(status = 'dropped'), 0),
		       COUNT(*)
		FROM drill_day`).Scan(&w.DrillDays, &w.DroppedDays, &w.WindowDays)
	if err != nil {
		return Window{}, fmt.Errorf("shadow: audit 3: %w", err)
	}
	return w, nil
}

// ModeDenominator is the fourth audit query: the gate's four numbers for ONE
// supply mode.
//
// M IS HERE SINCE 0002, AND IT IS A RECORD RATHER THAN A DERIVATION. It counts
// runs whose run-level conclusion agreed with GitHub's, and this ledger holds no
// GitHub-side conclusion anywhere — the separate file was paid for with exactly
// that. So the validator's verdict is WRITTEN into exec_invocation.comparison
// (the validator's final verdict, closed at two values) and read back here.
// Keeping M outside the ledger was not an option: a number computed elsewhere
// comes back as a bare "96%", which the gate's reporting rule forbids.
//
// WHAT IS STILL NOT HERE: whether a mismatch was EXPLAINED. The gate's third
// condition needs that and no column holds it — still an open question.
type ModeDenominator struct {
	SupplyMode exec.SupplyMode

	// Measured is false when the ledger has NO drill run in this mode. A
	// missing row is not a zero: it means the gate was never measured in that
	// mode, and the other mode's count cannot stand in for it.
	Measured bool

	Attempted int // A — drill runs attempted in this mode
	Completed int // D — the denominator: A that reached `completed`
	Excluded  int // X = A - D; each one costs the gate's own 5% budget
	// Matched is M: rows whose comparison is exec.ComparisonMatched. The
	// queries below BIND that constant rather than spelling 'matched', so a
	// rename in internal/exec cannot leave a silently-zero numerator here.
	Matched int

	// Open is how many of Excluded are rows that reached NO verdict: the
	// harness crashed, was killed, or is still running. They are a SUBSET of
	// Excluded, not a fifth number, and they are surfaced because a run that
	// vanished mid-flight is the one exclusion nobody chose.
	Open int

	// Unverdicted is how many of Completed are drill runs carrying NO
	// comparison. Since 0002 a completed drill CANNOT close without one, so
	// this can only be rows written before that migration — a finite set that
	// cannot grow. It is reported because those rows sit in D and never in M,
	// which drags the ratio DOWN; the direction is safe, but a silently
	// depressed score is still a score nobody can explain.
	Unverdicted int
}

// DenominatorByMode returns EXACTLY TWO entries, online then offline, whether
// or not the ledger has rows for both.
//
// The raw query returns one row per mode present, and a
// single row means the gate was never measured in that mode. A caller handed a
// one-element slice will iterate it without noticing, so this returns both with
// Measured saying which is real. The score is mode-homogeneous:
// D >= 20 is satisfied PER MODE and an offline exclusion cannot be paid out of
// an online budget.
//
// TWO THINGS IN THE SQL ARE LOAD-BEARING AND BOTH ARE OF THE SILENT CLASS.
//
// X is `SUM(outcome IS NULL OR outcome <> 'completed')`, not
// `SUM(outcome <> 'completed')`. Since 0002 `outcome` can be NULL, and
// `NULL <> 'completed'` is NULL, which SUM SKIPS. The old expression runs,
// returns rows and errors on nothing while UNDERCOUNTING X — and in a mode
// where every row is open it returns NULL, which prints as an empty cell.
//
// The COALESCEs are not decoration for the same reason: SUM over zero rows is
// NULL, and a NULL counter reads as something other than zero.
func (db *DB) DenominatorByMode(ctx context.Context) ([]ModeDenominator, error) {
	byMode := map[exec.SupplyMode]ModeDenominator{}
	rows, err := db.reader.QueryContext(ctx, `
		SELECT supply_mode,
		       COUNT(*)                                                  AS a_attempted,
		       COALESCE(SUM(outcome = 'completed'), 0)                   AS d_denominator,
		       COALESCE(SUM(outcome IS NULL OR outcome <> 'completed'),0) AS x_excluded,
		       COALESCE(SUM(comparison = ?), 0)                          AS m_matched,
		       COALESCE(SUM(outcome IS NULL), 0)                         AS open_rows,
		       COALESCE(SUM(outcome = 'completed' AND comparison IS NULL), 0) AS unverdicted
		FROM exec_invocation
		WHERE purpose = 'drill'
		GROUP BY supply_mode`, string(exec.ComparisonMatched))
	if err != nil {
		return nil, fmt.Errorf("shadow: audit 4: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m ModeDenominator
		if err := rows.Scan(&m.SupplyMode, &m.Attempted, &m.Completed, &m.Excluded,
			&m.Matched, &m.Open, &m.Unverdicted); err != nil {
			return nil, err
		}
		m.Measured = true
		byMode[m.SupplyMode] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ModeDenominator, 0, 2)
	for _, mode := range []exec.SupplyMode{exec.SupplyOnline, exec.SupplyOffline} {
		if m, ok := byMode[mode]; ok {
			out = append(out, m)
			continue
		}
		out = append(out, ModeDenominator{SupplyMode: mode})
	}
	return out, nil
}

// OpenRun is one row of the fifth audit query.
type OpenRun struct {
	InvocationID string
	Day          string
	SupplyMode   exec.SupplyMode
	Purpose      exec.Purpose
	StartedAt    time.Time
}

// OpenInvocations returns runs that reached NO verdict: outcome IS NULL.
//
// THIS ONE IS NOT REQUIRED TO BE EMPTY, and that is where it differs from the
// first three. A row here means the harness never came back — it crashed, was
// killed, or is still running. That is a STATE, not a defect: the row enters A
// and is paid out of X.
//
// It still may not go unread. An audit expected to be empty gets REPAIRED the
// once it fires; this one gets READ. Unread, it is the human-side twin of the
// x_excluded bug: an attempted run disappears without anyone lying. It is also
// the only trace such a run can have, because a crashed process cannot emit its
// own counter.
func (db *DB) OpenInvocations(ctx context.Context) ([]OpenRun, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT invocation_id, drill_day, supply_mode, purpose, started_at
		FROM exec_invocation
		WHERE outcome IS NULL
		ORDER BY started_at, invocation_id`)
	if err != nil {
		return nil, fmt.Errorf("shadow: audit 5: %w", err)
	}
	defer rows.Close()
	var out []OpenRun
	for rows.Next() {
		var r OpenRun
		var ms int64
		if err := rows.Scan(&r.InvocationID, &r.Day, &r.SupplyMode, &r.Purpose, &ms); err != nil {
			return nil, err
		}
		r.StartedAt = time.UnixMilli(ms)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CompletedDrillsWithoutComparison returns completed drill runs carrying no
// comparison verdict — the rows 0002's CHECK exempts.
//
// It exists because that exemption is the one thing in the schema that is
// scoped rather than absolute, and an exemption nobody can list is an exemption
// nobody can audit. Every row it returns was written under schema 1, when the
// column did not exist and the validator's output shape was not settled; the
// migration would not invent a verdict for them, because writing 'matched'
// fabricates the gate's numerator and writing 'mismatched' fabricates the third
// condition's subject.
//
// THE SET CANNOT GROW. exec_invocation_schema_floor refuses any INSERT claiming
// schema_version < 2, and schema_version is inside
// exec_invocation_write_once's column list so it cannot be lowered afterwards.
// These rows count toward A and D and never toward M, so they make the gate
// read WORSE — but a depressed score with no visible cause is its own defect,
// which is why this query exists rather than a comment.
func (db *DB) CompletedDrillsWithoutComparison(ctx context.Context) ([]string, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT invocation_id FROM exec_invocation
		WHERE purpose = 'drill' AND outcome = 'completed' AND comparison IS NULL
		ORDER BY started_at, invocation_id`)
	if err != nil {
		return nil, fmt.Errorf("shadow: grandfathered drills: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ExclusionCount is one refusal reason and how many drill runs it excluded, in
// one supply mode.
type ExclusionCount struct {
	SupplyMode exec.SupplyMode
	Outcome    exec.Outcome
	// Outcome is EMPTY when the run reached no verdict at all. That is a state,
	// not a missing value, and it is why this field is not a plain string join
	// key.
	// Reason is empty for deadline_exceeded, which names its own cause and
	// carries no refusal_reason, and for an unverdicted run, which has none.
	Reason exec.RefusalReason
	Count  int
}

// ExclusionsByReason breaks X down, per mode, by outcome and reason.
//
// The gate's report must carry A, D, M and X's BREAKDOWN BY REASON together,
// and a bare percentage is forbidden: a number without what it was measured
// over cannot be judged. This is the breakdown half.
func (db *DB) ExclusionsByReason(ctx context.Context) ([]ExclusionCount, error) {
	// `outcome IS NULL OR outcome <> 'completed'` — the same repair as
	// DenominatorByMode's x_excluded, and it must be made in BOTH places or the
	// breakdown stops summing to X. A run that reached no verdict is an
	// exclusion with an EMPTY outcome and an empty reason, and it is reported
	// as such rather than dropped.
	rows, err := db.reader.QueryContext(ctx, `
		SELECT supply_mode, outcome, refusal_reason, COUNT(*)
		FROM exec_invocation
		WHERE purpose = 'drill' AND (outcome IS NULL OR outcome <> 'completed')
		GROUP BY supply_mode, outcome, refusal_reason
		ORDER BY supply_mode, outcome, refusal_reason`)
	if err != nil {
		return nil, fmt.Errorf("shadow: exclusions: %w", err)
	}
	defer rows.Close()
	var out []ExclusionCount
	for rows.Next() {
		var c ExclusionCount
		var reason, outcome sql.NullString
		if err := rows.Scan(&c.SupplyMode, &outcome, &reason, &c.Count); err != nil {
			return nil, err
		}
		c.Outcome = exec.Outcome(outcome.String)
		c.Reason = exec.RefusalReason(reason.String)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DayCount is one day's run-axis totals.
type DayCount struct {
	Day        string
	SupplyMode exec.SupplyMode
	Attempted  int
	Completed  int
	Matched    int
}

// CountsByDay is why drill_day stores no counts: A and D are derivable per day
// by grouping the run axis, and a stored tally would be a second copy that has
// to be maintained by hand every round.
//
// It carries M since 0002: M's being a RECORD rather than a derivation does not
// revive the argument for storing tallies on drill_day, because the row itself
// carries the verdict and summing rows is not a second copy.
func (db *DB) CountsByDay(ctx context.Context) ([]DayCount, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT drill_day, supply_mode,
		       COUNT(*),
		       COALESCE(SUM(outcome = 'completed'), 0),
		       COALESCE(SUM(comparison = ?), 0)
		FROM exec_invocation
		WHERE purpose = 'drill'
		GROUP BY drill_day, supply_mode
		ORDER BY drill_day, supply_mode`, string(exec.ComparisonMatched))
	if err != nil {
		return nil, fmt.Errorf("shadow: counts by day: %w", err)
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var c DayCount
		if err := rows.Scan(&c.Day, &c.SupplyMode, &c.Attempted, &c.Completed, &c.Matched); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResidueCount is one invocation's leaked containers and volumes.
type ResidueCount struct {
	Kind  string
	Count int
}

// ResidueTotals counts the leak by kind (act leaks containers and
// volumes). The measured rate is one
// container and two volumes per FAILED job; this table only counts, and the
// cleanup policy belongs to the pool design.
func (db *DB) ResidueTotals(ctx context.Context) ([]ResidueCount, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM exec_residue GROUP BY kind ORDER BY kind`)
	if err != nil {
		return nil, fmt.Errorf("shadow: residue totals: %w", err)
	}
	defer rows.Close()
	var out []ResidueCount
	for rows.Next() {
		var c ResidueCount
		if err := rows.Scan(&c.Kind, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// The CANDIDATE axis's six audits.
//
// Same discipline as the run axis's audits: the query is written, the ruling is not made. The
// first five MUST come back empty on a sound ledger. The sixth is different in
// kind and is described at its own function.
//
// AND THE RULE THIS BLOCK LIVES UNDER, because these joins invite breaking it
// (it was MEASURED rather than argued):
//
//	reading                             A   D   M   ratio
//	DenominatorByMode (exec_invocation) 5   5   4   80.0%
//	the same query INNER JOINed
//	through drill_attempt               4   4   4   100.0%
//
// One unclaimed mismatched `online` run, and the JOIN ERASED IT. The query
// runs, rows come back, nothing errors, and the gate passes — a silently
// wrong answer from a query that looks healthy.
//
// So: the gate reads A/D/M/X from exec_invocation and from nowhere else. The
// candidate axis is a CROSS-CHECK, not a source, and the two readings being
// equal is precisely DrillRunsWithoutACandidate coming back empty — which is
// why it runs BEFORE the gate's numbers are read, the same ordering rule that
// puts the audits in front of retention.
//
// Nothing in this package offers the gate's numbers through this axis, and
// nothing should be added that does.
// ---------------------------------------------------------------------------

// CandidateRef names one candidate row.
type CandidateRef struct {
	CandidateID string
	Day         string
}

// SelectedCandidatesWithoutAttempts is the first candidate audit. MUST BE EMPTY: the
// selector promised a run and nothing was ever planned for it.
func (db *DB) SelectedCandidatesWithoutAttempts(ctx context.Context) ([]CandidateRef, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT c.candidate_id, c.drill_day FROM drill_candidate c
		WHERE c.disposition = ?
		  AND NOT EXISTS (SELECT 1 FROM drill_attempt a WHERE a.candidate_id = c.candidate_id)
		ORDER BY c.drill_day, c.candidate_id`, string(DispositionSelected))
	if err != nil {
		return nil, fmt.Errorf("shadow: candidate audit 1: %w", err)
	}
	defer rows.Close()
	var out []CandidateRef
	for rows.Next() {
		var r CandidateRef
		if err := rows.Scan(&r.CandidateID, &r.Day); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnpairedCandidate is one row of the second candidate audit, with the mode count that
// made it fire.
type UnpairedCandidate struct {
	CandidateID string
	Day         string
	Modes       int
}

// CandidatesNotPlannedInBothModes is the second candidate audit. MUST BE
// EMPTY: the mode schedule is PAIRED, so that a difference between the modes cannot be confused
// with a difference between corpora.
//
// PlanAttempts writes both modes in one transaction and takes no mode argument,
// so nothing this package exports can produce a row here. That is deliberate
// and it does not make the query redundant: it is what still speaks when the
// writer is not this package.
func (db *DB) CandidatesNotPlannedInBothModes(ctx context.Context) ([]UnpairedCandidate, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT c.candidate_id, c.drill_day, COUNT(DISTINCT a.supply_mode) AS modes
		FROM drill_candidate c
		LEFT JOIN drill_attempt a ON a.candidate_id = c.candidate_id
		WHERE c.disposition = ?
		GROUP BY c.candidate_id, c.drill_day
		HAVING COUNT(DISTINCT a.supply_mode) <> ?
		ORDER BY c.drill_day, c.candidate_id`,
		string(DispositionSelected), len(exec.SupplyModes()))
	if err != nil {
		return nil, fmt.Errorf("shadow: candidate audit 2: %w", err)
	}
	defer rows.Close()
	var out []UnpairedCandidate
	for rows.Next() {
		var r UnpairedCandidate
		if err := rows.Scan(&r.CandidateID, &r.Day, &r.Modes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AttemptRef names one planned attempt.
type AttemptRef struct {
	CandidateID string
	SupplyMode  exec.SupplyMode
	Day         string
}

// AttemptsWithoutInvocation is the third candidate audit. MUST BE EMPTY on a settled
// day: the attempt was planned and no invocation row was ever opened for it.
//
// THIS IS THE ONE GAP exec_invocation's two-phase row cannot cover, because
// there the row was never born. It is the reason drill_attempt exists at all:
// a run we selected and could not invoke must not look, in the ledger, like a
// run we never selected.
//
// A day still in progress will legitimately return rows. That is the same
// reading as OpenInvocations: a row is a STATE until the day closes.
func (db *DB) AttemptsWithoutInvocation(ctx context.Context) ([]AttemptRef, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT a.candidate_id, a.supply_mode, c.drill_day
		FROM drill_attempt a JOIN drill_candidate c ON c.candidate_id = a.candidate_id
		WHERE a.invocation_id IS NULL
		ORDER BY c.drill_day, a.candidate_id, a.supply_mode`)
	if err != nil {
		return nil, fmt.Errorf("shadow: candidate audit 3: %w", err)
	}
	defer rows.Close()
	var out []AttemptRef
	for rows.Next() {
		var r AttemptRef
		if err := rows.Scan(&r.CandidateID, &r.SupplyMode, &r.Day); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AttemptContradiction is one row of the fourth candidate audit: everything both sides
// say, so the disagreement can be read without a second query.
type AttemptContradiction struct {
	CandidateID    string
	InvocationID   string
	AttemptMode    exec.SupplyMode
	InvocationMode exec.SupplyMode
	CandidateDay   string
	InvocationDay  string
	Purpose        exec.Purpose
}

// AttemptsContradictingTheirInvocation is the fourth candidate audit. MUST BE EMPTY:
// the invocation an attempt points at disagrees with it about the supply mode,
// the day, or the purpose.
//
// LinkInvocation refuses all three at the write. This is the half that still
// speaks when the writer is not this package — the same division of labour
// exec_invocation_no_reverdict has with CloseInvocation's CAS.
func (db *DB) AttemptsContradictingTheirInvocation(ctx context.Context) ([]AttemptContradiction, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT a.candidate_id, a.invocation_id, a.supply_mode, e.supply_mode,
		       c.drill_day, e.drill_day, e.purpose
		FROM drill_attempt a
		JOIN drill_candidate c ON c.candidate_id = a.candidate_id
		JOIN exec_invocation e ON e.invocation_id = a.invocation_id
		WHERE e.supply_mode <> a.supply_mode OR e.drill_day <> c.drill_day OR e.purpose <> ?
		ORDER BY c.drill_day, a.candidate_id, a.supply_mode`, string(exec.PurposeDrill))
	if err != nil {
		return nil, fmt.Errorf("shadow: candidate audit 4: %w", err)
	}
	defer rows.Close()
	var out []AttemptContradiction
	for rows.Next() {
		var r AttemptContradiction
		if err := rows.Scan(&r.CandidateID, &r.InvocationID, &r.AttemptMode, &r.InvocationMode,
			&r.CandidateDay, &r.InvocationDay, &r.Purpose); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnclaimedRun is one row of the fifth candidate audit.
type UnclaimedRun struct {
	InvocationID string
	Day          string
	SupplyMode   exec.SupplyMode
}

// DrillRunsWithoutACandidate is the fifth candidate audit, and it is the one
// the measurement above is about. MUST BE EMPTY: a drill run exists that no candidate row accounts for.
//
// The drill package's single entry point removes the caller's ability to get the order wrong INSIDE the drill
// package; any other package can still build an exec.Request and call Run, and
// this is what catches that afterwards. The protection is structural in one
// place and audit-driven in the other, and the difference is written down
// rather than blurred (making Run accept only a ticket the drill package can
// mint is the repair that would close it, and it is still open).
//
// IT RUNS BEFORE THE GATE'S NUMBERS ARE READ. A row here is exactly the run
// that an INNER JOIN through drill_attempt would drop — 80.0% read honestly,
// 100.0% read through the join — so while this is non-empty the gate's ratio
// has an unaccounted run in it and must not be quoted.
func (db *DB) DrillRunsWithoutACandidate(ctx context.Context) ([]UnclaimedRun, error) {
	rows, err := db.reader.QueryContext(ctx, `
		SELECT e.invocation_id, e.drill_day, e.supply_mode FROM exec_invocation e
		WHERE e.purpose = ?
		  AND NOT EXISTS (SELECT 1 FROM drill_attempt a WHERE a.invocation_id = e.invocation_id)
		ORDER BY e.drill_day, e.invocation_id`, string(exec.PurposeDrill))
	if err != nil {
		return nil, fmt.Errorf("shadow: candidate audit 5: %w", err)
	}
	defer rows.Close()
	var out []UnclaimedRun
	for rows.Next() {
		var r UnclaimedRun
		if err := rows.Scan(&r.InvocationID, &r.Day, &r.SupplyMode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueuedCandidatesBefore is the sixth candidate audit, and it is NOT REQUIRED TO BE
// EMPTY. That is what separates it from the five above.
//
// A row does not mean something broke; it means a run is still waiting — either
// deferred by the day's cap or not yet concluded by GitHub. It is returned in
// GitHub's own created order, oldest first, which is the deferral order.
//
// IT STILL MAY NOT GO UNREAD. `deferred` is not terminal by design, and the
// only thing that keeps it from quietly becoming a permanent exclusion is
// somebody looking at this list. An audit expected to be empty gets REPAIRED
// the once it fires; this one gets READ — the same reading OpenInvocations
// takes on the run axis.
func (db *DB) QueuedCandidatesBefore(ctx context.Context, watermarkDay string) ([]Candidate, error) {
	if !ValidDayKey(watermarkDay) {
		return nil, fmt.Errorf("shadow: watermark day %q is not YYYY-MM-DD", watermarkDay)
	}
	rows, err := db.reader.QueryContext(ctx, `
		SELECT candidate_id, drill_day, examined_at, selector_rev, source,
		       repo, workflow_path, commit_sha, event_name,
		       github_run_id, github_run_attempt, github_created_at, github_conclusion,
		       disposition, disposition_detail
		FROM drill_candidate
		WHERE disposition IN (?, ?) AND drill_day < ?
		ORDER BY github_created_at IS NULL, github_created_at, candidate_id`,
		string(DispositionDeferred), string(DispositionNotConcluded), watermarkDay)
	if err != nil {
		return nil, fmt.Errorf("shadow: candidate audit 6: %w", err)
	}
	return scanCandidates(rows)
}
