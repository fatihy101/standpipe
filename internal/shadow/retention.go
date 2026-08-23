package shadow

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// GateWindowDays is the gate's own number: 14 drill-days must fall inside at
// most 21 CALENDAR days.
//
// It is here for exactly one purpose — to say what retention may NOT delete —
// and it is the only place in this package that hard-codes a gate constant.
// The horizon itself is NOT a constant: it is derived from this number and the
// ledger's own MAX(day), the same move the window rule requires and
// the same one that took the gap budget from the gate's own 7. A CHOSEN
// retention constant is the thing the first person who needs disk space lowers,
// and lowering it is invisible; a derived one cannot be lowered without
// changing the gate.
const GateWindowDays = 21

// childTables is the retention delete order, children first.
//
// Under PRAGMA foreign_keys = ON the reverse order is a constraint violation,
// not a style preference. An earlier Phase 0 design had exactly this defect — a
// retention step written parent-first that could never have run — and it is
// recorded here so that it is not repeated. Executed: deleting
// drill_day first, and deleting exec_invocation before its children, are both
// refused by the database.
//
// THIS IS AN FK ORDER AND IT IS NOT THE ORDER Retain's rules are about. That
// order is an AUDIT order — when the whole delete may run at all — and the row that defect
// destroyed did not violate any foreign key. Getting this list right buys
// nothing against that.
//
// drill_attempt IS FIRST SINCE 0003, and it was driven: with the candidate axis
// present and this list unchanged, `DELETE FROM exec_invocation` came back
// `FOREIGN KEY constraint failed (787)`. It is unlike the
// other six in one way that matters — it is a child of drill_candidate as WELL
// as of exec_invocation — so deleting it by invocation_id here is only half the
// job; purgeCandidates does the other half, and both halves are needed because
// the two parents can name different days on a defective ledger.
var childTables = []string{
	"drill_attempt",
	"exec_step_observation",
	"exec_job_result",
	"exec_image",
	"exec_residue",
	"exec_local_repository",
	"exec_note",
}

// RetainResult is what a retention pass that actually ran deleted, and the
// window it derived in order to be allowed to run.
//
// Window is the third audit query, taken inside the same transaction and
// BEFORE the deletes. It is reported rather than checked: query 3 returns
// counts, so unlike queries 1 and 2 it has no "must be empty" condition and
// cannot refuse anything. Its role here is the derivation, and reporting it is
// what makes the numbers the purge was permitted under readable afterwards.
type RetainResult struct {
	// Cutoff is the day key handed in. Everything with a day key strictly
	// BEFORE it was deleted.
	Cutoff string

	// LatestDay is MAX(drill_day.day) at the moment of the transaction, and
	// WindowStart is the first day of the gate's own window, derived from it.
	// A cutoff at or after WindowStart is refused.
	LatestDay   string
	WindowStart string

	Window Window

	Invocations int64 // exec_invocation rows deleted, with their children
	Candidates  int64 // drill_candidate rows deleted
	Attempts    int64 // drill_attempt rows deleted
	Days        int64 // drill_day rows deleted

	// There is no Zones field, and its absence is a statement: Retain NEVER
	// deletes from drill_day_zone. That row EXPLAINS a day key and has to
	// outlive it, which is why it has no foreign key onto drill_day
	// (a record that explains a key cannot be that key's child).
}

// RetentionRefused is the refusal, WITH the rows that caused it.
//
// The audit rule requires the offending rows to be REPORTED, not merely counted: a
// refusal that says "the audit fired" and not "these days" leaves the operator
// with the delete as the only cheap move again. Every field is filled whether
// or not it caused the refusal, so a caller reading the error sees the whole
// state the decision was made on.
//
// Unwrap returns ALL causes, so errors.Is answers truthfully for each one when
// two rules fire at once.
type RetentionRefused struct {
	// Causes are sentinels, in the rules' own order: the audit rule first, the
	// window rule second.
	Causes []error

	Cutoff      string
	LatestDay   string
	WindowStart string
	Window      Window

	// DrillRunsOnDroppedDays is audit 1's rows; RanDaysWithoutDrillRuns is
	// audit 2's. Audit 2's rows are the ones the retention rules were written
	// about: a childless
	// `ran` day is this call's easiest target and that audit's only subject.
	DrillRunsOnDroppedDays  []DrillOnDroppedDay
	RanDaysWithoutDrillRuns []string
}

func (e *RetentionRefused) Unwrap() []error { return e.Causes }

func (e *RetentionRefused) Error() string {
	var b strings.Builder
	b.WriteString("shadow: retention refused and deleted nothing")
	for _, c := range e.Causes {
		b.WriteString("; ")
		switch {
		case c == ErrRetentionAuditFired:
			fmt.Fprintf(&b, "%v: %d drill run(s) on dropped days, %d `ran` day(s) with no drill run",
				c, len(e.DrillRunsOnDroppedDays), len(e.RanDaysWithoutDrillRuns))
			if len(e.RanDaysWithoutDrillRuns) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(e.RanDaysWithoutDrillRuns, ", "))
			}
		case c == ErrCutoffInsideWindow:
			fmt.Fprintf(&b, "%v: cutoff %s is not before %s, the first day of the gate's %d-day window ending %s",
				c, e.Cutoff, e.WindowStart, GateWindowDays, e.LatestDay)
		case c == ErrWindowUndeterminable:
			fmt.Fprintf(&b, "%v: MAX(day) is %q and no %d-day window can be derived from it",
				c, e.LatestDay, GateWindowDays)
		default:
			fmt.Fprintf(&b, "%v", c)
		}
	}
	return b.String()
}

// Retain is the ONLY exported way to delete anything from this ledger.
//
// Retention is governed by three rules that no CHECK and no trigger can hold,
// because a table cannot know WHEN the statement that deletes it runs. The
// defect behind them was driven three times and it is not a lost row: PurgeDays
// deleted a childless `ran` day — the precise thing the second audit query
// exists to name — with no FK violation, no error and no warning, and the audit
// went GREEN afterwards. What was repaired was the evidence.
//
// So the purges are unexported and this call runs the rules itself, in one
// transaction:
//
//  1. Audits 1 and 2 run FIRST. If either returns a row, nothing is
//     deleted and the rows come back on *RetentionRefused.
//  2. The cutoff must be strictly before the first day of the gate's own
//     window, which is derived here from the ledger's MAX(day) and
//     GateWindowDays — not chosen, and not read from a clock.
//  3. A defective day is never repaired by deleting it, which is rule 1 seen
//     from the other side and is why the refusal names the days.
//
// WHY ONE TRANSACTION AND NOT TWO CALLS. Running the audits on the read pool
// and then purging would restore the ordering IN NAME ONLY: a defective row
// written between the audit and the delete is exactly the case the rule exists
// for, and it would be deleted by a purge that had "passed". The audits
// therefore run on the writer connection inside BEGIN IMMEDIATE, on the same
// snapshot the DELETEs will see. Since this package is single-writer, no writer
// of ours can interleave at all.
//
// THE CUTOFF IS A DAY KEY AND BOTH AXES ARE PURGED ON IT. Invocations are
// selected by drill_day, NOT by started_at. The gate's window is a DAY window,
// so a horizon expressed in wall-clock time would need the drill's timezone —
// which this package deliberately does not own — and any skew
// between the two would let retention take a run out of a day the gate is still
// counting. One key, one axis, no zone, no clock.
//
// It returns *RetentionRefused (via error) when a rule fires, a plain error
// when the cutoff is malformed or SQLite fails, and RetainResult when it ran.
func (db *DB) Retain(ctx context.Context, cutoffDay string) (RetainResult, error) {
	if !ValidDayKey(cutoffDay) {
		return RetainResult{}, fmt.Errorf("shadow: retention cutoff %q is not YYYY-MM-DD", cutoffDay)
	}

	var (
		res         RetainResult
		invRows     int64
		candRows    int64
		attemptRows int64
		dayRows     int64
	)
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		// Everything below reads and writes on ONE pinned connection inside
		// BEGIN IMMEDIATE. Order within the transaction is still the rules' order,
		// because the refusal has to be able to say which rule fired.
		refusal := &RetentionRefused{Cutoff: cutoffDay}

		dropped, err := drillRunsOnDroppedDays(ctx, tx)
		if err != nil {
			return err
		}
		silent, err := ranDaysWithoutDrillRuns(ctx, tx)
		if err != nil {
			return err
		}
		refusal.DrillRunsOnDroppedDays = dropped
		refusal.RanDaysWithoutDrillRuns = silent
		if len(dropped) > 0 || len(silent) > 0 {
			refusal.Causes = append(refusal.Causes, ErrRetentionAuditFired)
		}

		if refusal.Window, err = windowShape(ctx, tx); err != nil {
			return err
		}

		latest, windowStart, err := gateWindow(ctx, tx)
		if err != nil {
			return err
		}
		refusal.LatestDay, refusal.WindowStart = latest, windowStart
		switch {
		case windowStart == "":
			refusal.Causes = append(refusal.Causes, ErrWindowUndeterminable)
		case cutoffDay >= windowStart:
			// String comparison is sound only because the format is
			// fixed-width and zero-padded, which the column's GLOB enforces
			// and ValidDayKey has already checked for the cutoff.
			refusal.Causes = append(refusal.Causes, ErrCutoffInsideWindow)
		}

		if len(refusal.Causes) > 0 {
			return refusal
		}

		res = RetainResult{
			Cutoff:      cutoffDay,
			LatestDay:   latest,
			WindowStart: windowStart,
			Window:      refusal.Window,
		}
		// The audit-first order, with the candidate axis added, and
		// every step of it is a foreign key rather than a preference: the run
		// axis and its children, then the candidate axis and its attempts,
		// then the day rows both of them point at.
		var linkedAttempts, orphanedAttempts int64
		if invRows, linkedAttempts, err = purgeInvocations(ctx, tx, cutoffDay); err != nil {
			return err
		}
		if candRows, orphanedAttempts, err = purgeCandidates(ctx, tx, cutoffDay); err != nil {
			return err
		}
		attemptRows = linkedAttempts + orphanedAttempts
		dayRows, err = purgeDays(ctx, tx, cutoffDay)
		return err
	})
	if err != nil {
		// Nothing committed, so nothing is reported as deleted. The counts are
		// only copied in after COMMIT for that reason: a purge that rolled back
		// half way must not return the half.
		return RetainResult{}, err
	}
	res.Invocations, res.Candidates, res.Attempts, res.Days = invRows, candRows, attemptRows, dayRows
	return res, nil
}

// gateWindow derives the protected window from the LEDGER.
//
// windowStart is the first day of the gate's 21-day window ending at MAX(day):
// date(MAX(day), '-20 days'). A cutoff must be strictly BEFORE it, which makes
// the retained span at least 22 days — the window rule's "strictly longer than
// the window", with the boundary case on the safe side.
//
// MAX(day), NOT time.Now(). Three things follow and all three are wanted:
//
//   - no clock is read, so a clock that jumps — forward or back — cannot move
//     the protected window. This is the one wall-clock hazard this package's
//     day axis actually escapes;
//   - a ledger that has stopped being written protects its LAST 21 recorded
//     days, not a band of calendar days that contains no rows. A today-based
//     window on a stale ledger would leave the newest real days deletable;
//   - the empty ledger has no window at all, and that returns "" rather than a
//     default. Retention on a ledger with no day axis deletes nothing anyway
//     (exec_invocation.drill_day is NOT NULL and references it), so refusing
//     costs nothing and keeps "derived, not chosen" absolute.
//
// windowStart is "" — refuse — when MAX(day) is NULL, and ALSO when date()
// cannot read it. The column's GLOB checks the SHAPE and not the date, so
// '9999-99-99' is a storable day key; it would become MAX(day) by string order
// and no window could be derived from it. Failing closed there means one
// malformed day key stops retention until a human removes it, which is the
// direction this project takes every time: the alternative is deleting against
// a window we could not compute.
func gateWindow(ctx context.Context, q queryer) (latest, windowStart string, err error) {
	var maxDay, start sql.NullString
	// GateWindowDays-1: the window is inclusive of both ends, so the 21-day
	// window ending at MAX(day) starts 20 days earlier.
	offset := fmt.Sprintf("-%d days", GateWindowDays-1)
	if err := q.QueryRowContext(ctx,
		`SELECT MAX(day), date(MAX(day), ?) FROM drill_day`, offset).
		Scan(&maxDay, &start); err != nil {
		return "", "", fmt.Errorf("shadow: deriving the gate window: %w", err)
	}
	return maxDay.String, start.String, nil
}

// purgeInvocations deletes every invocation on a day strictly before cutoffDay,
// children first, on the caller's transaction.
//
// It does NOT touch drill_day. That is deliberate and it is the day row's whole
// value: even in a window whose runs have been purged, "that day dropped and
// this is why" survives — the one thing no other table can reconstruct.
//
// It does not touch drill_candidate either; purgeCandidates does, and it runs
// after this. attempts is how many drill_attempt rows went with the invocations,
// which is only PART of the total — see the comment at the count.
//
// UNEXPORTED SINCE 2026-08-23, and the retention PERIOD is still not decided
// here: it takes a cutoff, and Retain is what decides the cutoff is allowed.
func purgeInvocations(ctx context.Context, tx *sql.Conn, cutoffDay string) (invocations, attempts int64, err error) {
	for _, t := range childTables {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM `+t+` WHERE invocation_id IN
			 (SELECT invocation_id FROM exec_invocation WHERE drill_day < ?)`,
			cutoffDay)
		if err != nil {
			return 0, 0, fmt.Errorf("shadow: purging %s: %w", t, err)
		}
		// drill_attempt is the one child whose count REACHES THE CALLER, so it
		// is the one child that has to be counted here as well as in
		// purgeCandidates. The two statements delete disjoint sets and either
		// can be the one that takes a given row: this one takes attempts whose
		// INVOCATION is going, the other takes attempts whose CANDIDATE is.
		// Counting only one of them would report a number that is right on a
		// sound ledger and quietly short on any other, which is the shape of
		// defect this file exists to stop reporting.
		if t == "drill_attempt" {
			n, err := res.RowsAffected()
			if err != nil {
				return 0, 0, err
			}
			attempts += n
		}
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM exec_invocation WHERE drill_day < ?`, cutoffDay)
	if err != nil {
		return 0, 0, fmt.Errorf("shadow: purging exec_invocation: %w", err)
	}
	invocations, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	return invocations, attempts, nil
}

// purgeDays deletes day rows before cutoffDay that carry NO invocation, on the
// caller's transaction.
//
// THIS IS THE CALL THAT BROKE THE AUDIT-FIRST RULE AND IT IS WHY IT IS NO
// LONGER EXPORTED. A
// childless `ran` day is the second audit query's only subject and this
// statement's easiest target; deleting it makes that audit green by removing
// the evidence, with no FK violation, no error and no warning. Retain is now
// the only thing that can reach it, and Retain has already run that audit on
// this very transaction.
//
// The "no invocation" condition is not an optimisation, it is what makes this
// statement incapable of violating the foreign key regardless of the order it
// runs in. It is kept even though Retain fixes the order, because it is what
// holds if a future caller inside this package gets the order wrong.
//
// cutoffDay is compared as a string. That is only sound because the format is
// fixed-width and zero-padded, which the column's GLOB enforces.
func purgeDays(ctx context.Context, tx *sql.Conn, cutoffDay string) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		DELETE FROM drill_day
		WHERE day < ?
		  AND NOT EXISTS (SELECT 1 FROM exec_invocation e WHERE e.drill_day = drill_day.day)
		  AND NOT EXISTS (SELECT 1 FROM drill_candidate c WHERE c.drill_day = drill_day.day)`,
		cutoffDay)
	if err != nil {
		return 0, fmt.Errorf("shadow: purging drill_day: %w", err)
	}
	return res.RowsAffected()
}

// purgeCandidates deletes the candidate axis for days strictly before
// cutoffDay: the attempts first, then the candidates that own them.
//
// IT IS A SECOND PASS OVER drill_attempt AND BOTH PASSES ARE NEEDED. childTables
// already deleted the attempts that point at a purged INVOCATION; this deletes
// the ones that belong to a purged CANDIDATE. On a sound ledger the two sets
// coincide — an attempt's invocation and its candidate are on the same day, and
// LinkInvocation refuses anything else — but the ledger is not assumed sound
// here. It is exactly the assumption "these two rows must agree, so I only need
// to look at one" that the fourth candidate audit exists because
// nobody could make.
//
// A candidate with NO attempt at all is the common case rather than an edge:
// six of the seven dispositions never plan anything. The delete is therefore
// bounded by drill_day, not by the attempt table.
//
// UNEXPORTED, like the other two, and for the same reason (an ordering rule no schema can
// hold gets a single call site): Retain is the
// only thing that may reach it, and Retain has already run the audits on this
// very transaction.
func purgeCandidates(ctx context.Context, tx *sql.Conn, cutoffDay string) (candidates, attempts int64, err error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM drill_attempt WHERE candidate_id IN
		 (SELECT candidate_id FROM drill_candidate WHERE drill_day < ?)`,
		cutoffDay)
	if err != nil {
		return 0, 0, fmt.Errorf("shadow: purging drill_attempt by candidate: %w", err)
	}
	attempts, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}

	res, err = tx.ExecContext(ctx,
		`DELETE FROM drill_candidate WHERE drill_day < ?`, cutoffDay)
	if err != nil {
		return 0, 0, fmt.Errorf("shadow: purging drill_candidate: %w", err)
	}
	candidates, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	return candidates, attempts, nil
}

// AND drill_day_zone IS NEVER DELETED. There is deliberately no purge for it,
// and this comment is here so that the absence is a decision rather than an
// oversight.
//
// The row says which timezone a day key was drawn in (the drill's day boundary). It has
// to outlive the days it explains, or retention destroys the only record of how
// the SURVIVING keys were drawn — and that is also why the table carries no
// foreign key onto drill_day: with one, retention could not delete the named day
// at all and Retain would fail permanently. Driven, both directions, 2026-08-23.
//
// The table grows by one row per timezone change, which is an operator action
// requiring an explicit call. It does not need a horizon.
