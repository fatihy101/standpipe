package shadow

// This file is the drill candidate axis's rule table: every rule the shipped
// schema (migrations 0003 and 0004) holds on drill_day_zone, drill_candidate
// and drill_attempt, and the NAME of the constraint, trigger or index that
// holds it. internal/docgen renders it into docs/reference/drill-rules.md.
//
// A name is what makes a rule checkable. ruletable_test.go compares the names
// here with the objects the migrations actually build, in both directions,
// and for every rule held by a name it writes a row the rule forbids and
// requires SQLite to refuse it with an error attributed to that same name. A
// constraint violation reaches the operator under that name, so the reference
// and the error message use the same word.

// RuleHold says what holds a drill rule.
type RuleHold int

const (
	// HeldByName: a named CHECK constraint, trigger or index.
	HeldByName RuleHold = iota
	// HeldByStructure: a FOREIGN KEY or a UNIQUE column. Real, but SQLite
	// names no object of its own when it fires.
	HeldByStructure
	// HeldByAbsence: the rule IS the absence of a constraint.
	HeldByAbsence
	// HeldByOrder: an order, which no CHECK can hold. Code holds it and an
	// audit query reads it.
	HeldByOrder
)

// String describes the hold for the reference.
func (h RuleHold) String() string {
	switch h {
	case HeldByName:
		return "named object"
	case HeldByStructure:
		return "structural constraint"
	case HeldByAbsence:
		return "absence of a constraint"
	case HeldByOrder:
		return "order (code path)"
	}
	return "unknown"
}

// DrillRule is one row of the rule table.
type DrillRule struct {
	// Num is the rule number, contiguous from 1.
	Num int
	// Table is the table the rule is about.
	Table string
	// Rule is the rule, in one or two sentences.
	Rule string
	// Hold says what kind of thing holds the rule.
	Hold RuleHold
	// Objects are the shipped CHECK, trigger or index names that hold the
	// rule. Non-empty exactly when Hold is HeldByName.
	Objects []string
	// HeldBy describes, for every other hold, what carries the rule.
	HeldBy string
}

// DrillRules returns the candidate axis's rule table, in rule order.
func DrillRules() []DrillRule {
	return []DrillRule{
		// ---- drill_day_zone
		{Num: 1, Table: "drill_day_zone", Hold: HeldByAbsence,
			Rule:   "The interpretation of a day key is a separate record and has NO foreign key to `drill_day`. It must outlive the days it explains; with a foreign key, retention would delete the only record of which time zone the surviving keys were drawn in.",
			HeldBy: "The absence of a foreign key. `shadow.Retain` never deletes from this table."},
		{Num: 2, Table: "drill_day_zone", Hold: HeldByName, Objects: []string{"drill_day_zone_since_day_shape"},
			Rule: "`since_day` has the same shape as a day key (`YYYY-MM-DD`); a timestamp cannot be written."},
		{Num: 3, Table: "drill_day_zone", Hold: HeldByName, Objects: []string{"drill_day_zone_timezone_nonempty"},
			Rule: "`timezone` is an IANA zone name, not an offset, and cannot be empty."},
		{Num: 4, Table: "drill_day_zone", Hold: HeldByName, Objects: []string{"drill_day_zone_append_only"},
			Rule: "A zone row is append-only: rewriting it would retroactively change which calendar day every key since `since_day` names."},

		// ---- drill_candidate: identity and shape
		{Num: 5, Table: "drill_candidate", Hold: HeldByStructure,
			Rule:   "A candidate falls on a day the ledger has recorded.",
			HeldBy: "Foreign key `drill_candidate.drill_day` → `drill_day(day)`."},
		{Num: 6, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_selector_rev_nonempty"},
			Rule: "The identity of the selector that made the decision is recorded. The inputs are not enough; which selector looked at them is needed too."},
		{Num: 7, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_source_enum"},
			Rule: "`source` has two values: `github_history` (the drill's main path) and `synthetic` (a run the ref prober proposes that has no counterpart on GitHub)."},
		{Num: 8, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_commit_sha_len"},
			Rule: "`commit_sha` is exactly 40 characters."},
		{Num: 9, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_run_attempt_positive"},
			Rule: "`github_run_attempt ≥ 1`."},

		// ---- GitHub's verdict
		{Num: 10, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_github_conclusion_enum"},
			Rule: "`github_conclusion` is one of GitHub's nine conclusion values (including `startup_failure`), and never `in_progress`, which is a value of `status`. The set is deliberately not narrowed to the values observed so far: not having been observed is not evidence of absence."},
		{Num: 11, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_history_has_run_id", "drill_candidate_history_has_created_at", "drill_candidate_history_has_run_attempt"},
			Rule: "A history-path row carries `github_run_id`, `github_created_at` and `github_run_attempt`; a synthetic row carries none of them. The bond is two-way."},
		{Num: 12, Table: "drill_candidate", Hold: HeldByOrder,
			Rule:   "The deferral queue is in GitHub's own order: `github_created_at` ascending, oldest first. The sort key reads nothing from our side, so no ranking of ours can put the hard runs off indefinitely.",
			HeldBy: "The column itself; the deferral-queue audit query reads the order. No CHECK can hold an ORDER BY."},

		// ---- the closed disposition set
		{Num: 13, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_disposition_enum"},
			Rule: "`disposition` has seven values and none of them refers to our ability to run something. An eighth value is a schema change."},
		{Num: 14, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_history_dispositions"},
			Rule: "The prefilter cannot eliminate a history-path run. GitHub created that run; our filter disagreeing is a finding, not a reason to drop it."},
		{Num: 15, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_prefilter_dispositions_are_synthetic"},
			Rule: "`not_created` and `undecidable` exist only on the synthetic path. (Today rule 14's constraint refuses every violating row first; this one is defence in depth against a widened `source`.)"},
		{Num: 16, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_synthetic_never_enters_the_score"},
			Rule: "A synthetic candidate never enters the score: `selected` and `deferred` are both forbidden for it. Otherwise a verdict GitHub never gave would flow straight into the gate's numerator."},
		{Num: 17, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_selected_needs_conclusion", "drill_candidate_not_concluded_has_no_conclusion"},
			Rule: "`selected` requires a conclusion; `not_concluded` cannot carry one."},
		{Num: 18, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_exclusion_has_a_reason"},
			Rule: "Not being selected always carries a sentence; a reasonless exclusion cannot be written. `deferred` is included on purpose: a cap that does not say which cap is a hiding place."},
		{Num: 19, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_out_of_scope_detail_enum"},
			Rule: "The detail of `out_of_scope` is one of three closed words: `shadow_never`, `not_configured`, `github_internal_workflow`."},
		{Num: 20, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_github_internal_is_dynamic_path"},
			Rule: "A `dynamic/` workflow path ⟺ `out_of_scope` with `github_internal_workflow`, in both directions. It follows that `out_of_scope` beats `not_concluded` on such a path. The prefix test is case-sensitive."},

		// ---- once-ever, and append-only
		{Num: 21, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"idx_drill_candidate_selected_once"},
			Rule: "A GitHub run is selected at most once, ever. The gate counts distinct runs and its denominator query counts rows. (Partial UNIQUE index.)"},
		{Num: 22, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"drill_candidate_append_only"},
			Rule: "A candidate row is append-only: looking at a run again is a NEW row (`already_selected`); rewriting the old one would erase what the selector once said."},

		// ---- the two indexes
		{Num: 23, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"idx_drill_candidate_open"},
			Rule: "The deferral-queue audit query does not scan: the partial index covers `deferred` AND `not_concluded` together."},
		{Num: 24, Table: "drill_candidate", Hold: HeldByName, Objects: []string{"idx_drill_candidate_day"},
			Rule: "The per-day disposition breakdown reads from an index."},

		// ---- drill_attempt
		{Num: 25, Table: "drill_attempt", Hold: HeldByOrder,
			Rule:   "An attempt row is written BEFORE the process is invoked, with `invocation_id` NULL. A run we selected and failed to run must not look the same in the ledger as a run we never selected.",
			HeldBy: "`invocation_id` being nullable; the order is held by `RunDay` and read by the planned-but-uninvoked audit query. No CHECK can hold an order."},
		{Num: 26, Table: "drill_attempt", Hold: HeldByName, Objects: []string{"drill_attempt_supply_mode_enum"},
			Rule: "`supply_mode` has two values (`online`, `offline`) and the programme is paired: a selected candidate is attempted in both."},
		{Num: 27, Table: "drill_attempt", Hold: HeldByName, Objects: []string{"drill_attempt_only_for_selected"},
			Rule: "An attempt is planned only for a `selected` candidate. Otherwise the executor could run a candidate whose disposition says `out_of_scope`, and each half of the ledger would look sound on its own."},
		{Num: 28, Table: "drill_attempt", Hold: HeldByName, Objects: []string{"drill_attempt_write_once"},
			Rule: "After planning only `invocation_id` may be written; `candidate_id`, `supply_mode` and `planned_at` are write-once."},
		{Num: 29, Table: "drill_attempt", Hold: HeldByName, Objects: []string{"drill_attempt_invocation_write_once"},
			Rule: "`invocation_id` is written once. Re-pointing it at a second invocation would replace a run that went badly with a better one after the fact."},
		{Num: 30, Table: "drill_attempt", Hold: HeldByStructure,
			Rule:   "An invocation belongs to at most one attempt.",
			HeldBy: "`drill_attempt.invocation_id` is UNIQUE."},
	}
}

// LedgerTable is one table of shadow.db.
type LedgerTable struct {
	Name string
	// Axis groups the table: "bookkeeping", "day", "run" or "candidate".
	Axis    string
	Purpose string
}

// LedgerTables is the census of every table the shadow.db migrations build.
// ruletable_test.go compares it with sqlite_master in both directions, so a
// table that ships without an entry here fails a test instead of going
// unnoticed.
func LedgerTables() []LedgerTable {
	return []LedgerTable{
		{Name: "schema_version", Axis: "bookkeeping", Purpose: "Applied migrations and their checksums."},
		{Name: "drill_day", Axis: "day", Purpose: "One row for every day of the drill window, including why a day has no runs."},
		{Name: "exec_invocation", Axis: "run", Purpose: "One executor invocation, written in two phases: opened before the process, closed after it."},
		{Name: "exec_image", Axis: "run", Purpose: "The image used for each `runs-on` label of an invocation, with the configured source digest."},
		{Name: "exec_local_repository", Axis: "run", Purpose: "The HEAD of each local action mirror, read before the run, so a stale mirror is auditable."},
		{Name: "exec_job_result", Axis: "run", Purpose: "Per-job results of an invocation."},
		{Name: "exec_step_observation", Axis: "run", Purpose: "Step-level records observed during an invocation."},
		{Name: "exec_residue", Axis: "run", Purpose: "Containers and volumes an invocation left behind (counted, not cleaned)."},
		{Name: "exec_note", Axis: "run", Purpose: "Free-text notes the executor produced, stored verbatim."},
		{Name: "drill_day_zone", Axis: "candidate", Purpose: "The time zone day keys are drawn in, from a given day on."},
		{Name: "drill_candidate", Axis: "candidate", Purpose: "Every run the selector looked at, with its disposition and reason."},
		{Name: "drill_attempt", Axis: "candidate", Purpose: "The planned attempts for a selected candidate, one per supply mode."},
	}
}
