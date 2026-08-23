package shadow

import "errors"

var (
	// ErrSchemaOutdated means a shadow.db migration is pending. Deliberately
	// NOT store.ErrSchemaOutdated: the two databases migrate independently, and
	// an errors.Is that cannot tell them apart would let a caller run the wrong
	// migration in response to the wrong file.
	ErrSchemaOutdated = errors.New("shadow: schema outdated")

	// ErrSchemaNewer means shadow.db was written by a newer build. There is no
	// downgrade path.
	ErrSchemaNewer = errors.New("shadow: schema newer than this build")

	// ErrSchemaChecksum means schema_version disagrees with the embedded
	// migration source: the file was edited after it was applied, so the
	// database holds something other than what this build believes.
	ErrSchemaChecksum = errors.New("shadow: migration checksum mismatch")

	// ErrDuplicateInvocation means an invocation_id was already recorded.
	//
	// It is NOT swallowed and there is no ON CONFLICT DO NOTHING here. In
	// gateway.db a duplicate reservation is an expected replay; here it is a
	// defect in whoever mints the id, and a second record under one id would
	// mean two runs became one row.
	ErrDuplicateInvocation = errors.New("shadow: invocation already recorded")

	// ErrUnknownDay means a run named a drill_day that has no row.
	//
	// The foreign key produces it. That is the mechanism behind "a row exists
	// for every day of the window": a run cannot land on an unrecorded day, so
	// the day axis cannot silently acquire holes from the run axis.
	ErrUnknownDay = errors.New("shadow: run names a drill day that was never recorded")

	// ErrDayDowngrade means an attempt was made to relabel a `ran` day as
	// `dropped`. Forbidden by a trigger: it would invent an absence reason for
	// a day that was not absent.
	ErrDayDowngrade = errors.New("shadow: a ran day cannot be relabelled dropped")

	// ErrAppendOnly means an UPDATE was attempted on a table that only accepts
	// inserts.
	ErrAppendOnly = errors.New("shadow: the ledger is append-only")

	// ErrUnknownInvocation means a verdict named an invocation with no phase-1
	// row. Either OpenInvocation never ran, or the row has been purged.
	//
	// It is NOT an invitation to write the row now: a row created at close time
	// would be a trace of a process that had already ended, which is the exact
	// thing the two-phase shape exists to prevent.
	ErrUnknownInvocation = errors.New("shadow: no open invocation with that id")

	// ErrAlreadyClosed means a verdict was offered to a row that already has
	// one.
	//
	// Two mechanisms produce it and both are kept. The UPDATE carries
	// `AND outcome IS NULL`, so a lost race changes zero rows; and
	// exec_invocation_no_reverdict aborts the statement outright. The CAS gives
	// a distinguishable error, the trigger gives a message — and the trigger is
	// what still holds when the writer is not this package.
	ErrAlreadyClosed = errors.New("shadow: a verdict is written once and this invocation is already closed")

	// ErrComparisonRequired means a completed DRILL run was offered without a
	// comparison verdict.
	//
	// M is READ from that column and cannot be derived: this ledger holds no
	// GitHub-side conclusion anywhere, which is the price paid for the
	// separate file. Closing a compared run without the verdict would empty the gate's
	// numerator through an omission, silently.
	ErrComparisonRequired = errors.New("shadow: a completed drill run cannot close without a comparison verdict")

	// ErrComparisonForbidden means a comparison was offered on a row that was
	// never compared. An outage run has no GitHub side to compare against, and
	// an excluded run was not compared at all.
	ErrComparisonForbidden = errors.New("shadow: only a completed drill run may carry a comparison verdict")

	// ---------------------------------------------------------------------
	// The candidate axis.
	// ---------------------------------------------------------------------

	// ErrZoneAlreadyRecorded means a drill_day_zone row already exists for that
	// since_day. The table is append-only: rewriting a zone row retroactively
	// changes which calendar day every key since then names.
	ErrZoneAlreadyRecorded = errors.New("shadow: a timezone is already recorded from that day onward")

	// ErrDuplicateCandidate means a candidate_id was already recorded. As with
	// ErrDuplicateInvocation there is no ON CONFLICT DO NOTHING: two
	// examinations under one id would become one row.
	ErrDuplicateCandidate = errors.New("shadow: candidate already recorded")

	// ErrRunAlreadySelected means a GitHub run that an earlier day already
	// selected was offered as `selected` again.
	//
	// It is produced by the partial unique index, and it is what makes
	// the promotion gate's "20 DISTINCT workflow runs" equal to the denominator
	// query's ROW count: repeating one run on twenty days would otherwise be
	// indistinguishable from twenty runs. The caller's answer is a row with
	// disposition `already_selected` — which is a REJECTION carrying a reason,
	// not a silent skip.
	ErrRunAlreadySelected = errors.New("shadow: this GitHub run was already selected on an earlier day")

	// ErrUnknownCandidate means an attempt named a candidate with no row.
	ErrUnknownCandidate = errors.New("shadow: no candidate with that id")

	// ErrCandidateNotSelected means an attempt was planned for a candidate the
	// selector did not choose. A trigger holds it too: without that, the
	// disposition could say `out_of_scope` while the executor ran the run
	// anyway, and each half of the ledger would look sound alone.
	ErrCandidateNotSelected = errors.New("shadow: an attempt may only be planned for a selected candidate")

	// ErrAttemptAlreadyPlanned means the pair (candidate, supply mode) already
	// has a row.
	ErrAttemptAlreadyPlanned = errors.New("shadow: this attempt is already planned")

	// ErrUnknownAttempt means an invocation was offered to a (candidate, mode)
	// pair that was never planned. It is NOT an invitation to plan it now: a
	// plan written after the process is a trace of work nothing accounted for.
	ErrUnknownAttempt = errors.New("shadow: no planned attempt for that candidate and supply mode")

	// ErrAttemptAlreadyLinked means the attempt already points at an
	// invocation. The CAS on invocation_id IS NULL produces it, and
	// drill_attempt_invocation_write_once holds the same line from the database
	// side: re-pointing a planned attempt would let a bad run be replaced by a
	// better one after the fact.
	ErrAttemptAlreadyLinked = errors.New("shadow: this attempt already points at an invocation")

	// ErrAttemptContradiction means the invocation offered for an attempt
	// disagrees with it about the supply mode, the drill day, or the purpose.
	//
	// The fourth candidate-axis audit query finds these afterwards; this
	// refuses them at the write. Both are kept, because the audit is what still
	// speaks when the writer is not this package.
	ErrAttemptContradiction = errors.New("shadow: the invocation contradicts the attempt it was offered for")

	// The three below are Retain's refusals. They arrive wrapped in a
	// *RetentionRefused, which carries the ROWS as well as the reason, and
	// errors.Is answers for each of them separately when two fire at once.
	//
	// They are refusals, not failures: nothing was deleted, and in every case
	// the repair is a human one. A defective day is never repaired by deleting
	// it, which is why there is no force flag anywhere near them: the cheapest
	// way to turn an audit green is to delete what it names, and an override
	// would be that way with a spelling.

	// ErrRetentionAuditFired means audit 1 or 2 returned rows, so retention did
	// not run: the audits run before retention, and any row they return stops
	// it.
	ErrRetentionAuditFired = errors.New("shadow: an audit that must be empty returned rows, so nothing was deleted")

	// ErrCutoffInsideWindow means the cutoff was not strictly before the first
	// day of the gate's own window. Deleting inside it changes the
	// gate's denominator or its "last 7 drill-days" silently.
	ErrCutoffInsideWindow = errors.New("shadow: the retention cutoff falls inside the gate's own window")

	// ErrWindowUndeterminable means MAX(day) is missing or is not a date SQLite
	// can do arithmetic on, so the window could not be DERIVED. There is no
	// fallback constant on purpose: the horizon is derived from the gate's own
	// window, and a chosen one is exactly what that rule exists to prevent.
	ErrWindowUndeterminable = errors.New("shadow: the gate's window cannot be derived from the ledger")
)
