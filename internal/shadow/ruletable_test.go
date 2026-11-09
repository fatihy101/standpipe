package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The drill candidate axis's rule table, bound to the shipped schema.
//
// DrillRules() (drillrules.go) lists every rule the candidate-axis migrations
// hold, each with the NAME of the CHECK, trigger or index that holds it, and
// docs/reference/drill-rules.md is generated from it. A name is what makes a
// rule checkable, and this file checks it:
//
//   - the NAME SET, in both directions, against sqlite_master on a database
//     the shipped migrations actually built;
//   - which rules are held by no name at all (an absence, an order, or a
//     foreign key / UNIQUE column that SQLite does not name);
//   - for every rule that names something, an observed refusal ATTRIBUTED BY
//     SQLITE to that same object, plus rows the rule allows that must still
//     be writable.
//
// WHAT IT CANNOT DO: it does not read the rule SENTENCE. If a rule's text is
// rewritten to mean something else while its constraint keeps its name, this
// file stays green. The accept/reject cases are what bind the meaning.
// ---------------------------------------------------------------------------

// --------------------------------------------------------------------------
// Reading the schema the MIGRATIONS build.
// --------------------------------------------------------------------------

var (
	constraintBeforeCheck = regexp.MustCompile(`(?is)CONSTRAINT\s+([a-z_][a-z0-9_]*)\s*$`)
	raiseAbortLiteral     = regexp.MustCompile(`(?is)RAISE\s*\(\s*ABORT\s*,\s*'((?:[^']|'')*)'\s*\)`)
	createTableName       = regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:main\.)?([a-z_][a-z0-9_]*)`)
)

// candidateAxisTables is DERIVED from the migration that introduces the axis
// rather than typed here. 0003 introduces the axis, and if it ever adds
// a fourth table that table's constraints come under this test automatically.
// (0004 rebuilds drill_candidate; it introduces no table, so it adds nothing.)
func candidateAxisTables(t *testing.T) []string {
	t.Helper()
	var src string
	for _, m := range migrations {
		if strings.Contains(m.Name, "drill_candidate_axis") {
			src = m.Source
		}
	}
	if src == "" {
		t.Fatal("no embedded migration named *drill_candidate_axis*; the candidate axis cannot be located")
	}
	var out []string
	for _, m := range createTableName.FindAllStringSubmatch(stripSQLComments(src), -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	sort.Strings(out)
	if len(out) != 3 {
		t.Fatalf("the candidate-axis migration creates %v; the rule table is written about three tables", out)
	}
	return out
}

// axisObject is one nameable thing the schema holds a rule with.
type axisObject struct {
	kind  string // "check", "trigger", "index"
	table string
}

// schemaAxisObjects returns every NAMED constraint, trigger and explicit index
// on the candidate-axis tables, read out of the live database.
//
// It also returns the CHECKs that carry NO name. Naming is a REQUIREMENT on
// this axis, because an anonymous CHECK cannot appear in a table of names — and an anonymous
// CHECK is precisely the constraint that holds a rule nobody wrote down. It is
// counted here rather than assumed away.
func schemaAxisObjects(t *testing.T, db *DB, tables []string) (map[string]axisObject, []string) {
	t.Helper()
	objs := map[string]axisObject{}
	var anonymous []string

	inAxis := func(tbl string) bool { return contains(tables, strings.ToLower(tbl)) }

	rows, err := db.reader.QueryContext(context.Background(),
		`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE type IN ('table','trigger','index')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, name, tbl, ddl string
		if err := rows.Scan(&kind, &name, &tbl, &ddl); err != nil {
			t.Fatal(err)
		}
		if !inAxis(tbl) {
			continue
		}
		switch kind {
		case "table":
			named, anon := namedChecks(ddl)
			for _, n := range named {
				objs[n] = axisObject{kind: "check", table: tbl}
			}
			for _, body := range anon {
				anonymous = append(anonymous, tbl+": "+body)
			}
		case "trigger":
			objs[name] = axisObject{kind: "trigger", table: tbl}
		case "index":
			if ddl == "" {
				continue // an implicit index behind PRIMARY KEY / UNIQUE; it has no name of its own to write down
			}
			objs[name] = axisObject{kind: "index", table: tbl}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objs, anonymous
}

// namedChecks splits a CREATE TABLE's CHECKs into the named and the anonymous.
func namedChecks(ddl string) (names []string, anonymousBodies []string) {
	s := stripSQLComments(ddl)
	upper := strings.ToUpper(s)
	for i := 0; i+5 <= len(s); i++ {
		if upper[i:i+5] != "CHECK" {
			continue
		}
		if i > 0 && isIdentRune(rune(s[i-1])) {
			continue
		}
		j := i + 5
		for j < len(s) && (s[j] == ' ' || s[j] == '\n' || s[j] == '\t' || s[j] == '\r') {
			j++
		}
		if j >= len(s) || s[j] != '(' {
			continue
		}
		if m := constraintBeforeCheck.FindStringSubmatch(strings.TrimRight(s[:i], " \t\r\n")); m != nil {
			names = append(names, m[1])
		} else {
			end := j + 40
			if end > len(s) {
				end = len(s)
			}
			anonymousBodies = append(anonymousBodies, strings.Join(strings.Fields(s[j:end]), " "))
		}
		i = j
	}
	sort.Strings(names)
	return names, anonymousBodies
}

// refusalSignature derives, FROM THE LIVE SCHEMA, the text SQLite writes when
// `object` refuses a row. Nothing here is transcribed from the migration: the
// trigger's sentence is read back out of its own CREATE text and the index's
// columns out of PRAGMA index_info, so a renamed constraint or a reworded
// RAISE moves both sides at once and this file keeps binding the same thing.
//
// Measured against modernc.org/sqlite (2026-08-24), which prefixes everything
// with "constraint failed: " and appends the extended result code:
//
//	named CHECK   constraint failed: CHECK constraint failed: <name> (275)
//	trigger       constraint failed: <the RAISE(ABORT) sentence> (1811)
//	unique index  constraint failed: UNIQUE constraint failed: t.a, t.b (2067)
//	foreign key   constraint failed: FOREIGN KEY constraint failed (787)
//
// The last one is why rules 5 and 30 are a different class: SQLite does not
// name the object in a foreign-key error at all.
func refusalSignature(t *testing.T, db *DB, object string, objs map[string]axisObject) string {
	t.Helper()
	o, ok := objs[object]
	if !ok {
		t.Fatalf("no shipped object named %q on the candidate axis", object)
	}
	switch o.kind {
	case "check":
		return "CHECK constraint failed: " + object
	case "trigger":
		var ddl string
		if err := db.reader.QueryRowContext(context.Background(),
			`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, object).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		m := raiseAbortLiteral.FindStringSubmatch(ddl)
		if m == nil {
			t.Fatalf("trigger %s has no RAISE(ABORT, '...'), so nothing it refuses can be attributed to it", object)
		}
		return strings.ReplaceAll(m[1], "''", "'")
	case "index":
		rows, err := db.reader.QueryContext(context.Background(), `PRAGMA index_info(`+object+`)`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var cols []string
		for rows.Next() {
			var seq, cid int
			var col sql.NullString
			if err := rows.Scan(&seq, &cid, &col); err != nil {
				t.Fatal(err)
			}
			cols = append(cols, o.table+"."+col.String)
		}
		return "UNIQUE constraint failed: " + strings.Join(cols, ", ")
	}
	t.Fatalf("object %q has no known kind", object)
	return ""
}

// --------------------------------------------------------------------------
// Running one assertion.
// --------------------------------------------------------------------------

var errRollback = errors.New("shadow: rule assertion rolled back")

// tryRuleStmts runs stmts inside ONE BEGIN IMMEDIATE and always rolls back, so
// every assertion sees the same fixture and none of them can see each other.
// It returns nil when the whole sequence was accepted.
func tryRuleStmts(t *testing.T, db *DB, stmts ...string) error {
	t.Helper()
	var refused error
	err := db.writeTx(context.Background(), func(ctx context.Context, tx *sql.Conn) error {
		for _, s := range stmts {
			if _, e := tx.ExecContext(ctx, s); e != nil {
				refused = e
				return e
			}
		}
		return errRollback
	})
	if refused != nil {
		return refused
	}
	if errors.Is(err, errRollback) {
		return nil
	}
	if err != nil {
		t.Fatalf("the assertion harness itself failed: %v", err)
	}
	return nil
}

// --------------------------------------------------------------------------
// The registry: one entry per rule row, and how strongly it is bound.
// --------------------------------------------------------------------------

// Each binding states how it expects its rule to be held, and that statement
// is CHECKED against DrillRules() rather than believed: a rule that grows a
// name in the table while its binding still says "held by an order" (or the
// reverse) is a disagreement a name comparison alone cannot see.
const (
	heldByName      = HeldByName
	heldByStructure = HeldByStructure
	heldByAbsence   = HeldByAbsence
	heldByOrder     = HeldByOrder
)

// ruleCase is one row the schema must accept or refuse.
type ruleCase struct {
	name string
	sql  []string

	// accept is true when the row must be WRITABLE. A rule table checked only
	// with rows that fail proves the column is unusable, not that it is bound.
	accept bool

	// by names the shipped object that must refuse. It is ASSERTED TWICE: the
	// error SQLite returns has to attribute to it, and DrillRules()'s row has to
	// name it. That second half is the join between a rule NUMBER and a
	// shipped NAME, and it is the only mechanical link between the two halves
	// of the table that exists.
	by string

	// wording is used instead of `by` for the structural rules, where SQLite
	// writes a fixed phrase and names nothing.
	wording string
}

type ruleBinding struct {
	how RuleHold

	// shadowedBy names the constraint that refuses this rule's rows FIRST, so
	// this rule's own constraint cannot be observed firing. It is a RECORD, not
	// an excuse: a test asserting a shadowed constraint fires would assert
	// something false, and one that quietly skipped it would leave a rule with
	// no assertion and no confession. See assertPrefilterConstraintIsShadowed.
	shadowedBy string

	// cases are the accept/reject rows. Empty is allowed only with `nameOnly`.
	cases []ruleCase

	// custom is for the rules that are not a single INSERT: an order, a plan,
	// a PRAGMA.
	custom func(t *testing.T, db *DB, objs map[string]axisObject, row DrillRule)

	// nameOnly says WHY a row has no behavioural assertion. It is a confession,
	// printed in -v output, not a silent gap.
	nameOnly string
}

// --------------------------------------------------------------------------
// Fixture.
// --------------------------------------------------------------------------

const (
	ruleSHA  = "'0123456789abcdef0123456789abcdef01234567'"
	ruleDay2 = "2026-05-02"
	ruleInvA = "rule-inv-a"
	ruleInvB = "rule-inv-b"
)

// ruleLedger is candidateLedger plus a second recorded day and two closed
// invocations, because rules 29 and 30 need real exec_invocation rows to point
// an attempt at and the foreign key is not the subject of either.
func ruleLedger(t *testing.T) *DB {
	t.Helper()
	db := candidateLedger(t)
	mustDay(t, db, ruleDay2, DayRan, "")
	mustDrillRun(t, db, invSpec{id: ruleInvA, day: candDay})
	mustDrillRun(t, db, invSpec{id: ruleInvB, day: candDay})
	return db
}

// cand builds a drill_candidate INSERT. Every column is spelled at every call
// site on purpose: these assertions are about what a ROW may contain, and a
// builder with defaults would quietly supply the value under test.
func cand(id, day, source, path, runID, attempt, created, conclusion, disposition, detail string) string {
	return `INSERT INTO drill_candidate (candidate_id, drill_day, examined_at, selector_rev, source,
		repo, workflow_path, commit_sha, event_name,
		github_run_id, github_run_attempt, github_created_at, github_conclusion,
		disposition, disposition_detail) VALUES (` +
		q(id) + `, ` + q(day) + `, 1, 'selector-0001', ` + q(source) + `,
		'owner/name', ` + q(path) + `, ` + ruleSHA + `, 'push', ` +
		runID + `, ` + attempt + `, ` + created + `, ` + conclusion + `, ` +
		q(disposition) + `, ` + detail + `)`
}

func q(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// historyRow is the shape every accepted history-path row has, so a case that
// changes one field is visibly about that field.
func historyRow(id, disposition, detail string) string {
	return cand(id, candDay, "github_history", ".github/workflows/ci.yml", "101", "1", "10", "'success'", disposition, detail)
}

// okSelected is the row every rejection below is one field away from. It is
// asserted to be ACCEPTED under rule 5, so a change that quietly makes the
// baseline unwritable cannot turn every rejection assertion vacuous.
var okSelected = historyRow("c1", "selected", "NULL")

// --------------------------------------------------------------------------
// The registry itself.
// --------------------------------------------------------------------------

func drillRuleBindings() map[int]ruleBinding {
	return map[int]ruleBinding{
		// ---- drill_day_zone -------------------------------------------
		1: {
			how: heldByAbsence,
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				var n int
				if err := db.reader.QueryRowContext(context.Background(),
					`SELECT COUNT(*) FROM pragma_foreign_key_list('drill_day_zone')`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Errorf("drill_day_zone has %d foreign key(s). The rule IS the absence: with an FK, "+
						"retention cannot delete the day the zone row names and Retain fails permanently.", n)
				}
				// And the behaviour the absence buys: the interpretation
				// OUTLIVES the key it interprets. Driven inside one transaction
				// and rolled back, so the surviving row is observed rather than
				// inferred from the delete not erroring.
				surviving := -1
				err := db.writeTx(context.Background(), func(ctx context.Context, tx *sql.Conn) error {
					for _, stmt := range []string{
						`INSERT INTO drill_day_zone VALUES ('` + ruleDay2 + `', 'Europe/Istanbul')`,
						`DELETE FROM drill_day WHERE day = '` + ruleDay2 + `'`,
					} {
						if _, e := tx.ExecContext(ctx, stmt); e != nil {
							return e
						}
					}
					if e := tx.QueryRowContext(ctx,
						`SELECT COUNT(*) FROM drill_day_zone WHERE since_day = '`+ruleDay2+`'`).Scan(&surviving); e != nil {
						return e
					}
					return errRollback
				})
				if err != nil && !errors.Is(err, errRollback) {
					t.Errorf("deleting the day the zone row explains was refused: %v.\n"+
						"That is exactly the failure the missing FK exists to prevent: retention deletes days, "+
						"and a zone row that is the day's CHILD dies with the last day it explains.", err)
				}
				if surviving != 1 {
					t.Errorf("the zone row for %s did not survive the deletion of that day (count = %d)", ruleDay2, surviving)
				}
			},
		},
		2: {how: heldByName, cases: []ruleCase{
			{name: "a timestamp cannot be a day key", by: "drill_day_zone_since_day_shape",
				sql: []string{`INSERT INTO drill_day_zone VALUES ('2026-8-23', 'Europe/Istanbul')`}},
			{name: "a day key can", accept: true,
				sql: []string{`INSERT INTO drill_day_zone VALUES ('2026-08-23', 'Europe/Istanbul')`}},
		}},
		3: {how: heldByName, cases: []ruleCase{
			{name: "an empty zone is not an IANA name", by: "drill_day_zone_timezone_nonempty",
				sql: []string{`INSERT INTO drill_day_zone VALUES ('2026-08-23', '')`}},
		}},
		4: {how: heldByName, cases: []ruleCase{
			{name: "a zone row cannot be rewritten", by: "drill_day_zone_append_only",
				sql: []string{`UPDATE drill_day_zone SET timezone = 'UTC' WHERE since_day = '2026-01-01'`}},
		}},

		// ---- drill_candidate: identity and shape ----------------------
		5: {
			how: heldByStructure,
			cases: []ruleCase{
				{name: "a candidate on an unrecorded day", wording: "FOREIGN KEY constraint failed",
					sql: []string{cand("c1", "2026-07-01", "github_history", ".github/workflows/ci.yml", "101", "1", "10", "'success'", "selected", "NULL")}},
				{name: "a candidate on a recorded day", accept: true, sql: []string{okSelected}},
			},
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				assertForeignKey(t, db, "drill_candidate", "drill_day", "drill_day", "day")
			},
		},
		6: {how: heldByName, cases: []ruleCase{
			{name: "a decision with no selector behind it", by: "drill_candidate_selector_rev_nonempty",
				sql: []string{strings.Replace(okSelected, `'selector-0001'`, `''`, 1)}},
			{name: "and one with a selector revision", accept: true, sql: []string{okSelected}},
		}},
		7: {how: heldByName, cases: []ruleCase{
			{name: "a third source", by: "drill_candidate_source_enum",
				sql: []string{cand("c1", candDay, "guessed", ".github/workflows/ci.yml", "101", "1", "10", "'success'", "selected", "NULL")}},
		}},
		8: {how: heldByName, cases: []ruleCase{
			{name: "a short sha", by: "drill_candidate_commit_sha_len",
				sql: []string{strings.Replace(okSelected, ruleSHA, `'abc'`, 1)}},
		}},
		9: {how: heldByName, cases: []ruleCase{
			{name: "attempt zero", by: "drill_candidate_run_attempt_positive",
				sql: []string{cand("c1", candDay, "github_history", ".github/workflows/ci.yml", "101", "0", "10", "'success'", "selected", "NULL")}},
		}},

		// ---- GitHub's verdict -----------------------------------------
		10: {how: heldByName, cases: []ruleCase{
			{name: "startup_failure is GitHub's and 0004 shipped it", accept: true,
				sql: []string{strings.Replace(okSelected, `'success'`, `'startup_failure'`, 1)}},
			{name: "stale is unobserved and stays writable", accept: true,
				sql: []string{strings.Replace(okSelected, `'success'`, `'stale'`, 1)}},
			{name: "in_progress is a STATUS, never a conclusion", by: "drill_candidate_github_conclusion_enum",
				sql: []string{strings.Replace(okSelected, `'success'`, `'in_progress'`, 1)}},
			{name: "unmappable is OUR word and GitHub never writes it on a run", by: "drill_candidate_github_conclusion_enum",
				sql: []string{strings.Replace(okSelected, `'success'`, `'unmappable'`, 1)}},
		}},
		11: {how: heldByName, cases: []ruleCase{
			{name: "a history row without a run id", by: "drill_candidate_history_has_run_id",
				sql: []string{cand("c1", candDay, "github_history", ".github/workflows/ci.yml", "NULL", "1", "10", "'success'", "selected", "NULL")}},
			{name: "a history row without a created_at", by: "drill_candidate_history_has_created_at",
				sql: []string{cand("c1", candDay, "github_history", ".github/workflows/ci.yml", "101", "1", "NULL", "'success'", "selected", "NULL")}},
			{name: "a history row without a run attempt", by: "drill_candidate_history_has_run_attempt",
				sql: []string{cand("c1", candDay, "github_history", ".github/workflows/ci.yml", "101", "NULL", "10", "'success'", "selected", "NULL")}},
			{name: "a synthetic row CARRYING a run id — the bond is two-way", by: "drill_candidate_history_has_run_id",
				sql: []string{cand("c1", candDay, "synthetic", ".github/workflows/ci.yml", "101", "NULL", "NULL", "NULL", "not_created", "'the prefilter says no'")}},
		}},
		12: {
			how:      heldByOrder,
			nameOnly: "no CHECK can hold an ORDER BY; the column is the whole of the schema's part",
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				assertDeferralOrderIsGitHubs(t, db)
			},
		},

		// ---- the closed disposition set -------------------------------
		13: {how: heldByName, cases: []ruleCase{
			{name: "an eighth disposition naming OUR inability", by: "drill_candidate_disposition_enum",
				sql: []string{historyRow("c1", "cannot_reproduce", `'act does not support it'`)}},
		}},
		14: {how: heldByName, cases: []ruleCase{
			{name: "the prefilter eliminating a run GitHub created", by: "drill_candidate_history_dispositions",
				sql: []string{historyRow("c1", "not_created", `'our filter disagreed'`)}},
		}},
		15: {
			how:        heldByName,
			shadowedBy: "drill_candidate_history_dispositions",
			custom: func(t *testing.T, db *DB, objs map[string]axisObject, row DrillRule) {
				assertPrefilterConstraintIsShadowed(t, db, objs, row)
			},
		},
		16: {how: heldByName, cases: []ruleCase{
			{name: "a synthetic candidate SELECTED — a verdict laundered into M", by: "drill_candidate_synthetic_never_enters_the_score",
				sql: []string{cand("c1", candDay, "synthetic", ".github/workflows/ci.yml", "NULL", "NULL", "NULL", "'success'", "selected", "NULL")}},
			{name: "a synthetic candidate DEFERRED — a promise that can never be kept", by: "drill_candidate_synthetic_never_enters_the_score",
				sql: []string{cand("c1", candDay, "synthetic", ".github/workflows/ci.yml", "NULL", "NULL", "NULL", "NULL", "deferred", `'the day cap was full'`)}},
			{name: "and the other five stay writable for a synthetic row", accept: true,
				sql: []string{cand("c1", candDay, "synthetic", ".github/workflows/ci.yml", "NULL", "NULL", "NULL", "NULL", "out_of_scope", `'not_configured'`)}},
		}},
		17: {how: heldByName, cases: []ruleCase{
			{name: "selected before GitHub judged it", by: "drill_candidate_selected_needs_conclusion",
				sql: []string{cand("c1", candDay, "github_history", ".github/workflows/ci.yml", "101", "1", "10", "NULL", "selected", "NULL")}},
			{name: "not_concluded carrying a conclusion", by: "drill_candidate_not_concluded_has_no_conclusion",
				sql: []string{historyRow("c1", "not_concluded", `'GitHub has written nothing yet'`)}},
		}},
		18: {how: heldByName, cases: []ruleCase{
			{name: "a reasonless deferral", by: "drill_candidate_exclusion_has_a_reason",
				sql: []string{cand("c1", candDay, "github_history", ".github/workflows/ci.yml", "101", "1", "10", "NULL", "deferred", "NULL")}},
			{name: "a deferral that says WHICH cap", accept: true,
				sql: []string{historyRow("c1", "deferred", `'the day cap of 3 was full'`)}},
		}},
		19: {how: heldByName, cases: []ruleCase{
			{name: "out_of_scope in free text", by: "drill_candidate_out_of_scope_detail_enum",
				sql: []string{historyRow("c1", "out_of_scope", `'the operator said so'`)}},
			{name: "out_of_scope as one of the three words", accept: true,
				sql: []string{historyRow("c1", "out_of_scope", `'shadow_never'`)}},
		}},
		20: {how: heldByName, cases: []ruleCase{
			{name: "a dynamic/ path IS github_internal_workflow", accept: true,
				sql: []string{cand("c1", candDay, "github_history", "dynamic/pages/pages-build-deployment", "101", "1", "10", "'success'", "out_of_scope", `'github_internal_workflow'`)}},
			{name: "a dynamic/ path cannot be selected", by: "drill_candidate_github_internal_is_dynamic_path",
				sql: []string{cand("c1", candDay, "github_history", "dynamic/pages/x", "101", "1", "10", "'success'", "selected", "NULL")}},
			{name: "out_of_scope BEATS not_concluded on a dynamic/ path", by: "drill_candidate_github_internal_is_dynamic_path",
				sql: []string{cand("c1", candDay, "github_history", "dynamic/pages/x", "101", "1", "10", "NULL", "not_concluded", `'no verdict yet'`)}},
			{name: "a dynamic/ path claiming another out_of_scope meaning", by: "drill_candidate_github_internal_is_dynamic_path",
				sql: []string{cand("c1", candDay, "github_history", "dynamic/pages/x", "101", "1", "10", "'success'", "out_of_scope", `'shadow_never'`)}},
			{name: "OUR path claiming GitHub's machinery", by: "drill_candidate_github_internal_is_dynamic_path",
				sql: []string{historyRow("c1", "out_of_scope", `'github_internal_workflow'`)}},
			{name: "a run called BuildFailed is OURS", accept: true,
				sql: []string{cand("c1", candDay, "github_history", "BuildFailed", "101", "1", "10", "'startup_failure'", "selected", "NULL")}},
			{name: "DYNAMIC/ is not the class — substr, not a case-folded LIKE", accept: true,
				sql: []string{cand("c1", candDay, "github_history", "DYNAMIC/pages/x", "101", "1", "10", "'success'", "selected", "NULL")}},
		}},

		// ---- once-ever, and append-only -------------------------------
		21: {how: heldByName, cases: []ruleCase{
			{name: "the same run selected on a second day", by: "idx_drill_candidate_selected_once",
				sql: []string{okSelected,
					cand("c2", ruleDay2, "github_history", ".github/workflows/ci.yml", "101", "1", "10", "'success'", "selected", "NULL")}},
			{name: "seeing it again is a NEW row, not a second selection", accept: true,
				sql: []string{okSelected,
					cand("c2", ruleDay2, "github_history", ".github/workflows/ci.yml", "101", "1", "10", "'success'", "already_selected", `'selected on 2026-05-01'`)}},
		}},
		22: {how: heldByName, cases: []ruleCase{
			{name: "re-examination rewriting the old row", by: "drill_candidate_append_only",
				sql: []string{okSelected, `UPDATE drill_candidate SET disposition = 'out_of_scope' WHERE candidate_id = 'c1'`}},
		}},

		// ---- the two indexes ------------------------------------------
		23: {
			how: heldByName,
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				assertOpenIndexCoversBothHalves(t, db)
			},
		},
		24: {
			how: heldByName,
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				assertDayIndexIsRead(t, db)
			},
		},

		// ---- drill_attempt --------------------------------------------
		25: {
			how:      heldByOrder,
			nameOnly: "no CHECK can hold an ORDER; the schema's part is that invocation_id is NULLABLE",
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				assertAttemptIsWrittenBeforeInvocation(t, db)
			},
		},
		26: {how: heldByName, cases: []ruleCase{
			{name: "a third supply mode", by: "drill_attempt_supply_mode_enum",
				sql: []string{okSelected, `INSERT INTO drill_attempt VALUES ('c1', 'hybrid', 5, NULL)`}},
			{name: "and the programme is paired: both of the two", accept: true,
				sql: []string{okSelected,
					`INSERT INTO drill_attempt VALUES ('c1', 'online', 5, NULL)`,
					`INSERT INTO drill_attempt VALUES ('c1', 'offline', 5, NULL)`}},
		}},
		27: {how: heldByName, cases: []ruleCase{
			{name: "an attempt for a candidate that was NOT selected", by: "drill_attempt_only_for_selected",
				sql: []string{historyRow("c9", "out_of_scope", `'shadow_never'`),
					`INSERT INTO drill_attempt VALUES ('c9', 'online', 5, NULL)`}},
			{name: "an attempt for a candidate that does not exist at all", wording: "FOREIGN KEY constraint failed",
				sql: []string{`INSERT INTO drill_attempt VALUES ('ghost', 'online', 5, NULL)`}},
		}},
		28: {how: heldByName, cases: []ruleCase{
			{name: "re-pointing an attempt at another candidate", by: "drill_attempt_write_once",
				sql: []string{okSelected, `INSERT INTO drill_attempt VALUES ('c1', 'online', 5, NULL)`,
					`UPDATE drill_attempt SET candidate_id = 'c1' WHERE candidate_id = 'c1'`}},
			{name: "rewriting planned_at", by: "drill_attempt_write_once",
				sql: []string{okSelected, `INSERT INTO drill_attempt VALUES ('c1', 'online', 5, NULL)`,
					`UPDATE drill_attempt SET planned_at = 9 WHERE candidate_id = 'c1'`}},
		}},
		29: {how: heldByName, cases: []ruleCase{
			{name: "the FIRST invocation id is writable", accept: true,
				sql: []string{okSelected, `INSERT INTO drill_attempt VALUES ('c1', 'online', 5, NULL)`,
					`UPDATE drill_attempt SET invocation_id = ` + q(ruleInvA) + ` WHERE candidate_id = 'c1'`}},
			{name: "a second one is not", by: "drill_attempt_invocation_write_once",
				sql: []string{okSelected, `INSERT INTO drill_attempt VALUES ('c1', 'online', 5, NULL)`,
					`UPDATE drill_attempt SET invocation_id = ` + q(ruleInvA) + ` WHERE candidate_id = 'c1'`,
					`UPDATE drill_attempt SET invocation_id = ` + q(ruleInvB) + ` WHERE candidate_id = 'c1'`}},
		}},
		30: {
			how: heldByStructure,
			cases: []ruleCase{
				{name: "one invocation claimed by two attempts", wording: "UNIQUE constraint failed: drill_attempt.invocation_id",
					sql: []string{okSelected,
						`INSERT INTO drill_attempt VALUES ('c1', 'online', 5, ` + q(ruleInvA) + `)`,
						`INSERT INTO drill_attempt VALUES ('c1', 'offline', 5, ` + q(ruleInvA) + `)`}},
			},
			custom: func(t *testing.T, db *DB, _ map[string]axisObject, _ DrillRule) {
				assertUniqueColumn(t, db, "drill_attempt", "invocation_id")
			},
		},
	}
}

// --------------------------------------------------------------------------
// 0. The rule table's own shape.
// --------------------------------------------------------------------------

// drillRulesByNum checks DrillRules() is numbered contiguously from 1 and
// returns it indexed.
func drillRulesByNum(t *testing.T) map[int]DrillRule {
	t.Helper()
	rules := DrillRules()
	if len(rules) == 0 {
		t.Fatal("DrillRules() is empty; every assertion below is vacuous")
	}
	out := map[int]DrillRule{}
	for i, r := range rules {
		if r.Num != i+1 {
			t.Fatalf("DrillRules() numbers are not contiguous from 1: position %d is numbered %d", i+1, r.Num)
		}
		if strings.TrimSpace(r.Rule) == "" {
			t.Errorf("rule %d has no sentence", r.Num)
		}
		switch r.Hold {
		case HeldByName:
			if len(r.Objects) == 0 {
				t.Errorf("rule %d is held by a name and names nothing", r.Num)
			}
			if r.HeldBy != "" {
				t.Errorf("rule %d is held by a name and also carries a HeldBy description; the name is the description", r.Num)
			}
		default:
			if len(r.Objects) != 0 {
				t.Errorf("rule %d is held by %s and names objects %v; if a constraint really holds it, "+
					"its hold is stale and the row understates what ships", r.Num, r.Hold, r.Objects)
			}
			if strings.TrimSpace(r.HeldBy) == "" {
				t.Errorf("rule %d is held by %s and does not say what carries it", r.Num, r.Hold)
			}
		}
		out[r.Num] = r
	}
	return out
}

// --------------------------------------------------------------------------
// 1. The names, in both directions.
// --------------------------------------------------------------------------

// TestDrillRuleTableAndSchemaNameTheSameObjects compares DrillRules()'s
// object names with what the migrations build.
//
// Both directions fail. A rule naming an object the schema no longer has is a
// stale guard that reads exactly like a guard; an object in the schema that no
// rule names is a rule nobody wrote down, which is the failure a copied DDL
// listing could not report at all because a missing line appears in no diff.
func TestDrillRuleTableAndSchemaNameTheSameObjects(t *testing.T) {
	db := newLedger(t)
	rules := drillRulesByNum(t)
	tables := candidateAxisTables(t)
	objs, anonymous := schemaAxisObjects(t, db, tables)

	if len(objs) == 0 {
		t.Fatal("no named constraint, trigger or index was found on the candidate axis at all; the scanner is broken and every assertion below is vacuous")
	}

	// An unnamed CHECK cannot appear in a table of names, so it is a rule held
	// by something the reference structurally cannot mention.
	for _, a := range anonymous {
		t.Errorf("the candidate axis carries an ANONYMOUS CHECK: %s...\n"+
			"Every rule on this axis is held by NAME. A CHECK with no name cannot be written into the rule "+
			"table, cannot be grepped for, and prints the rebuild table's name at the operator when it fires.", a)
	}

	claimed := map[string]int{}
	for _, n := range sortedIntKeys(rules) {
		r := rules[n]
		if !contains(tables, r.Table) {
			t.Errorf("rule %d is about table %q, which is not on the candidate axis %v", n, r.Table, tables)
		}
		for _, name := range r.Objects {
			if prev, dup := claimed[name]; dup {
				t.Errorf("DrillRules() names %s in both rule %d and rule %d; one name cannot hold two rules "+
					"or the error message stops telling the operator which one came undone", name, prev, n)
			}
			claimed[name] = n
			o, ok := objs[name]
			if !ok {
				t.Errorf("rule %d names %s, which the shipped schema has no constraint, trigger or index for.\n"+
					"  rule: %s", n, name, r.Rule)
				continue
			}
			if o.table != r.Table {
				t.Errorf("rule %d is about %s and names %s, which lives on %s", n, r.Table, name, o.table)
			}
		}
	}
	for _, name := range sortedKeysOf(objs) {
		if _, ok := claimed[name]; !ok {
			t.Errorf("the shipped schema holds %s %s on %s, and no rule in DrillRules() names it.\n"+
				"A constraint holding a rule nobody wrote down is as much a defect as a rule naming a "+
				"constraint that no longer exists. Add the rule, then run `go generate ./...`.",
				objs[name].kind, name, objs[name].table)
		}
	}

	// The binding registry covers exactly the rules the table has, and agrees
	// with it about what holds each one.
	bindings := drillRuleBindings()
	for _, n := range sortedIntKeys(rules) {
		b, ok := bindings[n]
		if !ok {
			t.Errorf("rule %d has no binding in this file:\n  %s\n"+
				"Add it to drillRuleBindings(), with a behavioural case if the rule can be written and "+
				"refused, and with an explicit nameOnly reason if it cannot.", n, rules[n].Rule)
			continue
		}
		if b.how != rules[n].Hold {
			t.Errorf("rule %d is held by %s according to DrillRules() and by %s according to its binding. "+
				"One of the two is wrong about what carries the rule.", n, rules[n].Hold, b.how)
		}
	}
	for _, n := range sortedIntKeys(bindings) {
		if _, ok := rules[n]; !ok {
			t.Errorf("this file binds a rule %d that DrillRules() does not have. The table dropped a rule "+
				"and the test kept enforcing it; decide which side was right.", n)
		}
	}
}

// --------------------------------------------------------------------------
// 2. The meanings.
// --------------------------------------------------------------------------

// TestDrillRuleTableMeaningsHold writes, for every rule, rows the rule allows
// and rows it forbids, against the schema the migrations build.
//
// Each rejection is asserted TWICE over: SQLite's own error has to attribute
// the refusal to a named object, and DrillRules()'s row for that rule has to
// name the same object. The second half is the only mechanical link between a
// rule NUMBER and a shipped NAME, and without it the two halves of the table
// could drift apart with nothing to see.
func TestDrillRuleTableMeaningsHold(t *testing.T) {
	rules := drillRulesByNum(t)
	bindings := drillRuleBindings()
	base := newLedger(t)
	objs, _ := schemaAxisObjects(t, base, candidateAxisTables(t))

	asserted := 0
	for _, n := range sortedIntKeys(bindings) {
		row, ok := rules[n]
		if !ok {
			continue // already reported by the test above
		}
		b := bindings[n]
		t.Run(fmt.Sprintf("rule%02d", n), func(t *testing.T) {
			db := ruleLedger(t)
			for _, c := range b.cases {
				asserted++
				err := tryRuleStmts(t, db, c.sql...)
				switch {
				case c.accept && err != nil:
					t.Errorf("rule %d, %s: the schema REFUSED a row the rule allows: %v\n  rule: %s", n, c.name, err, row.Rule)
				case c.accept:
					// accepted, as it must be
				case err == nil:
					t.Errorf("rule %d, %s: the schema ACCEPTED it. The rule is not held.\n  rule: %s", n, c.name, row.Rule)
				case c.by != "":
					if !contains(row.Objects, c.by) {
						t.Errorf("rule %d does not name %s, and that is the object the schema refuses this row with. "+
							"Either the rule moved to another row or the row names the wrong constraint.\n  row names: %v", n, c.by, row.Objects)
						continue
					}
					want := refusalSignature(t, base, c.by, objs)
					if !strings.Contains(err.Error(), want) {
						t.Errorf("rule %d, %s: refused by something other than %s.\n  want the error to contain: %q\n  got: %v",
							n, c.name, c.by, want, err)
					}
				case c.wording != "":
					if !strings.Contains(err.Error(), c.wording) {
						t.Errorf("rule %d, %s: want the error to contain %q, got: %v", n, c.name, c.wording, err)
					}
				}
			}
			if b.custom != nil {
				b.custom(t, db, objs, row)
			}
			if len(b.cases) == 0 && b.custom == nil {
				t.Errorf("rule %d has no assertion at all", n)
			}
			if b.nameOnly != "" {
				t.Logf("rule %d is not bound by a refusal: %s", n, b.nameOnly)
			}
		})
	}
	t.Logf("%d accept/reject rows asserted against the schema the migrations built, over %d rules", asserted, len(bindings))
}

// --------------------------------------------------------------------------
// The custom bindings.
// --------------------------------------------------------------------------

// --------------------------------------------------------------------------
// The custom bindings.
// --------------------------------------------------------------------------

func assertForeignKey(t *testing.T, db *DB, table, from, toTable, toCol string) {
	t.Helper()
	rows, err := db.reader.QueryContext(context.Background(),
		`SELECT "table", "from", "to" FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, f string
		var to sql.NullString
		if err := rows.Scan(&tbl, &f, &to); err != nil {
			t.Fatal(err)
		}
		if tbl == toTable && f == from && (to.String == toCol || !to.Valid) {
			return
		}
	}
	t.Errorf("%s.%s has no foreign key to %s(%s). The rule is held by the key itself: SQLite names no "+
		"object in a foreign-key error, so this PRAGMA is the only thing that can see it.", table, from, toTable, toCol)
}

func assertUniqueColumn(t *testing.T, db *DB, table, column string) {
	t.Helper()
	rows, err := db.reader.QueryContext(context.Background(), `SELECT name, origin FROM pragma_index_list(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type idx struct{ name, origin string }
	var all []idx
	for rows.Next() {
		var i idx
		if err := rows.Scan(&i.name, &i.origin); err != nil {
			t.Fatal(err)
		}
		all = append(all, i)
	}
	for _, i := range all {
		if i.origin != "u" {
			continue
		}
		var cols []string
		r2, err := db.reader.QueryContext(context.Background(), `SELECT name FROM pragma_index_info(?)`, i.name)
		if err != nil {
			t.Fatal(err)
		}
		for r2.Next() {
			var c sql.NullString
			if err := r2.Scan(&c); err != nil {
				r2.Close()
				t.Fatal(err)
			}
			cols = append(cols, c.String)
		}
		r2.Close()
		if len(cols) == 1 && cols[0] == column {
			return
		}
	}
	t.Errorf("%s.%s is not UNIQUE; PRAGMA index_list shows %v", table, column, all)
}

// assertDeferralOrderIsGitHubs is rule 12, and it is a BEHAVIOUR because no
// CHECK can hold an order. The rule is that the deferral queue is sorted by
// GitHub's own created_at, oldest first, and reads nothing from our side — a
// ranking that looked at us could put the hard runs off indefinitely.
func assertDeferralOrderIsGitHubs(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	// Inserted NEWEST FIRST and with candidate ids that sort the other way, so
	// a query that lost its ORDER BY, or sorted by rowid or by id, gives a
	// different answer than a query that reads GitHub's clock.
	mustExamine(t, db, candDay,
		candSpec{id: "aaa-newest", runID: 9001, disposition: DispositionDeferred, detail: "the day cap was full",
			createdAt: time.UnixMilli(1_690_000_000_000)},
		candSpec{id: "mmm-middle", runID: 9002, disposition: DispositionDeferred, detail: "the day cap was full",
			createdAt: time.UnixMilli(1_650_000_000_000)},
		candSpec{id: "zzz-oldest", runID: 9003, disposition: DispositionNotConcluded, detail: "GitHub has written nothing yet",
			createdAt: time.UnixMilli(1_600_000_000_000)},
	)
	got, err := db.QueuedCandidatesBefore(ctx, ruleDay2)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range got {
		ids = append(ids, c.CandidateID)
	}
	want := []string{"zzz-oldest", "mmm-middle", "aaa-newest"}
	if !equalSets(ids, want) {
		t.Errorf("the deferral queue came back %v, want %v — GitHub's created order, oldest first.\n"+
			"Any other order is a ranking of OURS, and a ranking of ours can put the hard runs off forever.", ids, want)
	}
	// And it spans both dispositions, which is what makes it a queue rather
	// than two lists.
	seen := map[Disposition]bool{}
	for _, c := range got {
		seen[c.Disposition] = true
	}
	if !seen[DispositionDeferred] || !seen[DispositionNotConcluded] {
		t.Errorf("the queue returned only %v; the deferral-queue audit query reads deferred AND not_concluded together", seen)
	}
}

// assertPrefilterConstraintIsShadowed RECORDS a fact rather than asserting a
// falsehood, and the distinction is the point.
//
// drill_candidate_prefilter_dispositions_are_synthetic cannot be observed
// firing today. It forbids `not_created` / `undecidable` on a row whose source
// is not `synthetic`, and `source` has exactly TWO values — so the only row
// that can violate it is a `github_history` row, which
// drill_candidate_history_dispositions refuses first. It is defence in depth
// against a widened `source`, not a defect, and a test asserting it fires
// would be asserting something false.
//
// So: the constraint must EXIST under the name DrillRules() gives it, every row that
// violates it must be refused (by the other one), and the shadowing must still
// be COMPLETE. The completeness check is the live half — if a third source
// value appears, this fails and says the constraint has become observable.
func assertPrefilterConstraintIsShadowed(t *testing.T, db *DB, objs map[string]axisObject, row DrillRule) {
	t.Helper()
	const shadow = "drill_candidate_history_dispositions"
	for _, n := range row.Objects {
		if _, ok := objs[n]; !ok {
			t.Errorf("rule %d names %s and the schema does not have it", row.Num, n)
		}
	}
	if len(CandidateSources()) != 2 {
		t.Errorf("drill_candidate.source now has %d values. The record below says "+
			"drill_candidate_prefilter_dispositions_are_synthetic is SHADOWED, and that was true only while "+
			"`github_history` was the single non-synthetic source. It may now fire on its own — which is what "+
			"it was written for. Re-measure and update this test's record rather than deleting it.",
			len(CandidateSources()))
	}
	for _, disp := range []string{"not_created", "undecidable"} {
		err := tryRuleStmts(t, db, historyRow("c1", disp, `'the prefilter said so'`))
		if err == nil {
			t.Fatalf("a %s row on the history path was ACCEPTED; both constraints are gone", disp)
		}
		got := err.Error()
		if !strings.Contains(got, "CHECK constraint failed: "+shadow) {
			t.Errorf("a %s row on the history path is now refused by something other than %s: %v.\n"+
				"If it is refused by drill_candidate_prefilter_dispositions_are_synthetic, the shadowing has "+
				"ENDED and this record is stale — update it; the constraint is no longer unobservable.", disp, shadow, err)
		}
	}
}

// planOf returns EXPLAIN QUERY PLAN's detail lines.
//
// It runs on the WRITER pool deliberately. A plan taken on a read-pool
// connection immediately after a DDL change made on the writer can describe
// the OLD schema: driven 2026-08-24, EXPLAIN QUERY PLAN on the reader named an
// index that had just been dropped, and a plain query on the same connection
// afterwards forced the reload and changed the answer. A measurement taken the
// first way would have been reported as fact.
func planOf(t *testing.T, db *DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.writer.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

func planNames(plan []string, needle string) bool {
	for _, l := range plan {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// seedOpenCandidates writes rows so the plans below are not measured on an
// empty table. The planner's choice happens to be structural here (there is
// no sqlite_stat1 without ANALYZE), but a plan measured on an empty table is
// thinner evidence than the claim, and this is the honest version of the same
// measurement.
func seedOpenCandidates(ctx context.Context, t *testing.T, tx *sql.Conn, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		disp, concl, detail := "deferred", "'success'", `'the day cap was full'`
		switch i % 4 {
		case 1:
			disp, concl, detail = "not_concluded", "NULL", `'GitHub has written nothing yet'`
		case 2:
			disp, concl, detail = "already_selected", "'success'", `'selected earlier'`
		}
		stmt := cand(fmt.Sprintf("seed-%d", i), candDay, "github_history", ".github/workflows/ci.yml",
			strconv.Itoa(20000+i), "1", strconv.Itoa(1000+i), concl, disp, detail)
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}
}

// assertOpenIndexCoversBothHalves is rule 23, and it is deliberately split into
// what is STRUCTURAL and what is MEASURED, because only one of the two is a
// statement about the shipped code.
//
//  1. Structural: the partial index's WHERE clause covers `deferred` AS WELL
//     AS `not_concluded`. An index that covers half the query it exists for
//     leaves the other half unindexed, and the half an early design left out
//     was `deferred`, the disposition that turns into a permanent exclusion
//     when nobody reads it. Read out of the index's
//     own CREATE text, so it cannot drift.
//
//  2. Measured, on a seeded table, with the reference spelling of the
//     deferral-queue audit query (literal dispositions, `ORDER BY
//     github_created_at`) against a counterfactual ledger carrying a NARROW
//     index over `not_concluded` only. The comparison is retained as a
//     comparison rather than as a sentence.
//
//  3. And the part rule 23 does NOT say: the query the PACKAGE runs —
//     QueuedCandidatesBefore — does not use this index, for two independent
//     reasons measured 2026-08-24. Recorded here so nobody reads rule 23 as a
//     statement about shipped code. It is a record, not an endorsement: if the
//     query is repaired this assertion fails and asks for the record to move.
func assertOpenIndexCoversBothHalves(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()

	var idxSQL string
	if err := db.reader.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_drill_candidate_open'`).Scan(&idxSQL); err != nil {
		t.Fatalf("idx_drill_candidate_open: %v", err)
	}
	for _, half := range []Disposition{DispositionDeferred, DispositionNotConcluded} {
		if !strings.Contains(idxSQL, "'"+string(half)+"'") {
			t.Errorf("idx_drill_candidate_open does not cover %q:\n  %s\n"+
				"the deferral-queue audit query selects BOTH, and the half an index leaves out is the half that "+
				"becomes a hiding place.", half, idxSQL)
		}
	}

	// --- the measurement, and its counterfactual -----------------------
	const docQuery6 = `SELECT candidate_id, drill_day, disposition, repo, workflow_path, github_run_id
		FROM drill_candidate
		WHERE disposition IN ('deferred', 'not_concluded') AND drill_day < '2026-09-01'
		ORDER BY github_created_at`

	shipped := db
	if err := inWriteTx(t, shipped, func(ctx context.Context, tx *sql.Conn) error {
		seedOpenCandidates(ctx, t, tx, 400)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	shippedPlan := planOf(t, shipped, docQuery6)
	if !planNames(shippedPlan, "idx_drill_candidate_open") {
		t.Errorf("with the shipped index, the deferral-queue audit query does not use it: %v", shippedPlan)
	}
	if planNames(shippedPlan, "TEMP B-TREE") {
		t.Errorf("with the shipped index, the deferral-queue audit query still materialises a sort: %v", shippedPlan)
	}

	// The NARROW index, built by replacing the shipped one on its own
	// ledger. The DDL is not transcribed beyond the WHERE clause: everything
	// else about the table comes from the migrations.
	narrow := ruleLedger(t)
	if err := inWriteTx(t, narrow, func(ctx context.Context, tx *sql.Conn) error {
		seedOpenCandidates(ctx, t, tx, 400)
		if _, err := tx.ExecContext(ctx, `DROP INDEX idx_drill_candidate_open`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`CREATE INDEX idx_drill_candidate_open ON drill_candidate(github_created_at) WHERE disposition = 'not_concluded'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	narrowPlan := planOf(t, narrow, docQuery6)
	if planNames(narrowPlan, "idx_drill_candidate_open") {
		t.Errorf("with the narrow index, the deferral-queue audit query still uses it: %v.\n"+
			"Rule 23 rests on a narrow index NOT being usable for a query that reads both halves.", narrowPlan)
	}
	if !planNames(narrowPlan, "TEMP B-TREE") {
		t.Errorf("with the narrow index, the sort is not materialised: %v; the "+
			"measurement no longer reproduces and the claim needs re-taking", narrowPlan)
	}

	// --- and what rule 23 does NOT say about the shipped query ---------
	//
	// QueuedCandidatesBefore binds the dispositions as PARAMETERS, and SQLite
	// cannot prove `disposition IN (?, ?)` implies the partial index's own
	// WHERE; it also sorts by `github_created_at IS NULL, github_created_at,
	// candidate_id`, whose leading expression is not an index column. Either
	// alone is enough to lose the index. Measured 2026-08-24.
	const shippedQuery6 = `SELECT candidate_id FROM drill_candidate
		WHERE disposition IN (?, ?) AND drill_day < ?
		ORDER BY github_created_at IS NULL, github_created_at, candidate_id`
	live := planOf(t, shipped, shippedQuery6, string(DispositionDeferred), string(DispositionNotConcluded), "2026-09-01")
	if planNames(live, "idx_drill_candidate_open") {
		t.Errorf("QueuedCandidatesBefore NOW uses idx_drill_candidate_open: %v.\n"+
			"That is an improvement and this assertion is the record that it used not to. Delete the record, "+
			"and note in docs/reference/drill-rules.md that rule 23 now holds for the shipped query too.", live)
	}
}

// assertDayIndexIsRead is rule 24. The index is read by Candidates(day), which
// IS shipped, so unlike rule 23 the measurement and the code agree.
func assertDayIndexIsRead(t *testing.T, db *DB) {
	t.Helper()
	const shippedByDay = `SELECT candidate_id, drill_day, examined_at, selector_rev, source,
		repo, workflow_path, commit_sha, event_name,
		github_run_id, github_run_attempt, github_created_at, github_conclusion,
		disposition, disposition_detail
		FROM drill_candidate WHERE drill_day = ?
		ORDER BY github_created_at IS NULL, github_created_at, candidate_id`
	plan := planOf(t, db, shippedByDay, candDay)
	if !planNames(plan, "idx_drill_candidate_day") {
		t.Errorf("the shipped per-day read does not use idx_drill_candidate_day: %v", plan)
	}
	// And the disposition rollup the rule's sentence describes. There is no
	// shipped query for it today — said plainly rather than implied — so this
	// half measures the query the SENTENCE describes, not one the package runs.
	rollup := planOf(t, db, `SELECT disposition, COUNT(*) FROM drill_candidate WHERE drill_day = ? GROUP BY disposition`, candDay)
	if !planNames(rollup, "idx_drill_candidate_day") {
		t.Errorf("the in-day disposition rollup does not read the index: %v", rollup)
	}
}

// assertAttemptIsWrittenBeforeInvocation is rule 25. What the schema carries is
// only that invocation_id is NULLABLE; the ORDER — plan first, invoke second —
// is held by RunDay and read by the planned-but-uninvoked audit query, and no
// CHECK can hold an order. Both halves are asserted so the row's `—` is not a blank.
func assertAttemptIsWrittenBeforeInvocation(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()

	var notnull int
	if err := db.reader.QueryRowContext(ctx,
		`SELECT "notnull" FROM pragma_table_info('drill_attempt') WHERE name = 'invocation_id'`).Scan(&notnull); err != nil {
		t.Fatal(err)
	}
	if notnull != 0 {
		t.Error("drill_attempt.invocation_id is NOT NULL. A row that cannot be written before the process " +
			"is invoked makes a selected-but-never-invoked run indistinguishable from a run never selected.")
	}

	mustExamine(t, db, candDay, candSpec{id: "planned", runID: 7001})
	if err := db.PlanAttempts(ctx, "planned", time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatalf("PlanAttempts: %v", err)
	}
	open, err := db.AttemptsWithoutInvocation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("audit 3 sees %d planned-but-uninvoked attempts, want 2 (the programme is paired).\n"+
			"This is the one gap exec_invocation's two-phase row cannot catch, because there the row is never born.", len(open))
	}
}

// --------------------------------------------------------------------------
// 3. The census: every shipped table is listed.
// --------------------------------------------------------------------------

// TestLedgerTablesAreTheShippedTables answers the question a rule table
// cannot: what about a table that is simply ABSENT from it?
//
// A wrong value in a listing is findable by diff. A MISSING TABLE appears in
// no diff at all, because it has no line to compare against. So the shipped
// tables are enumerated OUTWARD from sqlite_master and compared with
// LedgerTables() in both directions, and the candidate-axis group must be
// exactly the tables the candidate-axis migration creates.
func TestLedgerTablesAreTheShippedTables(t *testing.T) {
	db := newLedger(t)

	rows, err := db.reader.QueryContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var shipped []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		shipped = append(shipped, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(shipped) == 0 {
		t.Fatal("the migrations built no tables at all; the census is vacuous")
	}

	listed := map[string]LedgerTable{}
	var candidateAxis []string
	for _, lt := range LedgerTables() {
		if _, dup := listed[lt.Name]; dup {
			t.Errorf("LedgerTables() lists %q twice", lt.Name)
		}
		listed[lt.Name] = lt
		if strings.TrimSpace(lt.Purpose) == "" || strings.TrimSpace(lt.Axis) == "" {
			t.Errorf("LedgerTables() entry %q has no axis or purpose", lt.Name)
		}
		if lt.Axis == "candidate" {
			candidateAxis = append(candidateAxis, lt.Name)
		}
	}

	for _, name := range shipped {
		if _, ok := listed[name]; !ok {
			t.Errorf("the shipped schema has a table %q that LedgerTables() does not list.\n"+
				"A listing is silent about what it lacks, so this is caught here rather than by a person "+
				"later. Add the table with its purpose, then run `go generate ./...`.", name)
		}
	}
	for name := range listed {
		if !contains(shipped, name) {
			t.Errorf("LedgerTables() lists %q and the migrations build no such table", name)
		}
	}

	sort.Strings(candidateAxis)
	if want := candidateAxisTables(t); !equalSets(candidateAxis, want) {
		t.Errorf("LedgerTables() puts %v on the candidate axis; the candidate-axis migration creates %v", candidateAxis, want)
	}
	for _, r := range DrillRules() {
		if lt, ok := listed[r.Table]; !ok || lt.Axis != "candidate" {
			t.Errorf("rule %d is about %q, which LedgerTables() does not list on the candidate axis", r.Num, r.Table)
		}
	}
}

func sortedIntKeys[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
