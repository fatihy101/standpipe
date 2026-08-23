package shadow

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/fatihy101/standpipe/internal/exec"
)

// shadow.db's CHECK literals are a HAND COPY of internal/exec's closed sets.
// The direction has been safe — Invocation.Validate checks against exec, so a
// divergence fails loudly at INSERT — but nothing DERIVED, and gateway.db's
// event_type enum is hand-written in five places of which four fail silently.
// That trap is one of the two reasons shadow.db exists at all.
//
// THIS TEST IS WRITTEN FROM sqlite_master OUTWARD, NOT FROM THE ENUMERATORS
// INWARD, and the difference is the whole point. Iterating the enumerators we
// happen to know about would pass for an eighth closed set added later and give
// no signal at all about it — this project's most repeated defect shape. So the
// schema is scanned for every closed-vocabulary CHECK it contains, and a column
// with no Go source of truth is a FAILURE rather than a gap.
//
// Verified: modernc.org/sqlite returns the original CREATE text, comments
// included, so the literals are read back out of the live database rather than
// out of the embedded migration file.

// vocabularySource is where a column's members come from on the Go side.
type vocabularySource struct {
	// members is the set as Go holds it.
	members []string

	// exemption is empty when the column is DERIVED from internal/exec. A
	// non-empty value says why this column is shadow's own vocabulary and not a
	// transcription — it is written down so the exemption is visible in the
	// failure message rather than silently absent from the table.
	exemption string

	// guard is empty for an UNCONDITIONAL vocabulary. A non-empty value says
	// the column is closed only under a condition, and names it exactly as the
	// schema spells it — e.g. `disposition <> 'out_of_scope'`.
	//
	// It is asserted, not decorative. drill_candidate.disposition_detail is
	// free text for six dispositions and a closed set for the seventh; a guard
	// silently changed to `disposition <> 'selected'` would leave both sides
	// agreeing about the MEMBERS while the vocabulary bound a different set of
	// rows. Registering the members without the guard would check half of it.
	guard string
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

// schemaVocabularies is the whole contract, one line per checked column.
func schemaVocabularies() map[string]vocabularySource {
	// exec_step_observation admits a narrower set than the run and job columns.
	// It is derived from shadow's OWN predicate rather than from exec, because
	// the narrowing is the schema's decision — but it is still derived, so widening
	// exec.Conclusions without touching either side is caught.
	var stepConclusions []string
	for _, c := range exec.Conclusions() {
		if ValidStepConclusion(c) {
			stepConclusions = append(stepConclusions, string(c))
		}
	}
	sort.Strings(stepConclusions)

	return map[string]vocabularySource{
		// --- transcriptions of internal/exec's seven closed sets ----------
		"exec_invocation.purpose":        {members: strs(exec.Purposes())},
		"exec_invocation.supply_mode":    {members: strs(exec.SupplyModes())},
		"exec_invocation.outcome":        {members: strs(exec.Outcomes())},
		"exec_invocation.refusal_reason": {members: strs(exec.RefusalReasons())},
		"exec_invocation.run_conclusion": {members: strs(exec.RunConclusions())},
		"exec_invocation.comparison":     {members: strs(exec.Comparisons())},
		"exec_job_result.conclusion":     {members: strs(exec.Conclusions())},
		"drill_attempt.supply_mode":      {members: strs(exec.SupplyModes())},

		// GitHub's own NINE, which is RunConclusions and not Conclusions. The
		// two sets are no longer nested: `unmappable` is OUR word for a
		// conclusion we could not map and GitHub never writes it on a run,
		// while `startup_failure` is GitHub's and no engine produces it. A
		// caller filtering one set to obtain the other is wrong in both
		// directions, and one of those directions ABORTS AN INSERT.
		"drill_candidate.github_conclusion": {members: strs(exec.RunConclusions())},

		// --- shadow's own vocabularies, each with its reason --------------
		"exec_step_observation.conclusion": {
			members: stepConclusions,
			exemption: "the schema's own narrowing: the STEP column admits neither action_required " +
				"nor stale. It is NOT derived from internal/exec and must not be folded into " +
				"exec_job_result.conclusion. shadow.ValidStepConclusion states it once.",
		},
		"drill_day.status": {
			members:   []string{string(DayDropped), string(DayRan)},
			exemption: "the day axis is shadow's own; internal/exec has no DayStatus.",
		},
		"drill_day.drop_reason": {
			members:   strs(DropReasons()),
			exemption: "the day axis is shadow's own; derived from shadow.DropReasons().",
		},
		"exec_job_result.source": {
			members:   []string{string(SourceExecutor), string(SourceModel)},
			exemption: "the validator's executor/model distinction is shadow's own record, not an exec vocabulary.",
		},
		"exec_residue.kind": {
			members:   []string{"container", "volume"},
			exemption: "the container/volume leak kinds are shadow's own; Residue.Kind is a plain string and Verdict.Validate is the Go side.",
		},
		"drill_candidate.source": {
			members: strs(CandidateSources()),
			exemption: "the candidate axis is shadow's own; internal/exec knows nothing about where a run " +
				"was proposed from. Derived from shadow.CandidateSources().",
		},
		"drill_candidate.disposition_detail": {
			members: strs(OutOfScopeDetails()),
			guard:   "disposition <> 'out_of_scope'",
			exemption: "the candidate axis is shadow's own. " +
				"Derived from shadow.OutOfScopeDetails(). It is GUARDED because the column is a CLASS " +
				"under `out_of_scope` and a SENTENCE everywhere else: `deferred` has to say WHICH cap, " +
				"and no two caps are the same words. Closing it everywhere would delete that, and " +
				"leaving it open under `out_of_scope` is what let THREE meanings — the operator's " +
				"`shadow: never`, not configured, and GitHub's own machinery — share one enum value " +
				"separated by nothing a query can read.",
		},
		"drill_candidate.disposition": {
			members: strs(Dispositions()),
			exemption: "the candidate axis is shadow's own. Derived from " +
				"shadow.Dispositions() — and the members are only half of what this set means: the RULE " +
				"is carried by what it does NOT contain. There is no value meaning \"we cannot reproduce " +
				"it\", so a run we cannot replay has to become a candidate, be attempted and land in X " +
				"rather than being dropped at selection. An eighth member is a schema change and is " +
				"reviewed as one, which is what makes the rule unbreakable by accident.",
		},
	}
}

// A CHECK is a closed VOCABULARY when its whole body is `col IN (...)` or
// `col IS NULL OR col IN (...)`. Anything else that contains an IN list is a
// RULE about the column, and rules are checked as SUBSETS instead.
//
// 0004 added a third shape, and it is a shape rather than a special case:
//
//	<guard_col> <> '<lit>' OR (<col> IS NOT NULL AND <col> IN (...))
//
// a GUARDED VOCABULARY — a column that is closed under a condition and free
// text otherwise. drill_candidate.disposition_detail is the first: it names a
// CLASS when the disposition is `out_of_scope` and carries a SENTENCE for the
// other five non-selected dispositions.
//
// The `<col> IS NOT NULL AND` half is not noise to be tolerated, it is the
// reason the constraint binds at all: `col IN (...)` evaluates to NULL when col
// is NULL and SQLITE PASSES A CHECK THAT EVALUATES TO NULL. A guarded
// vocabulary written without it is a vocabulary with a hole exactly where the
// value is absent. The peel below is the mirror of nullShape's, which exists
// for the same SQL rule pointing the other way.
//
// A column that has an UNCONDITIONAL vocabulary elsewhere is never promoted to
// a guarded one: `source <> 'github_history' OR disposition IN (...)` is a RULE
// about disposition, whose own vocabulary is stated on the column, and reading
// it as a second vocabulary would replace a subset check with an equality
// check against a deliberately smaller list.
var (
	vocabShape = regexp.MustCompile(`(?is)^\s*([a-z_][a-z0-9_]*)\s+IN\s*\((.*)\)\s*$`)
	nullShape  = regexp.MustCompile(`(?is)^\s*([a-z_][a-z0-9_]*)\s+IS\s+NULL\s+OR\s+(.*)$`)
	guardShape = regexp.MustCompile(`(?is)^\s*([a-z_][a-z0-9_]*\s*(?:<>|!=)\s*'(?:[^']|'')*')\s+OR\s+(.*)$`)
	notNull    = regexp.MustCompile(`(?is)^\s*([a-z_][a-z0-9_]*)\s+IS\s+NOT\s+NULL\s+AND\s+(.*)$`)
	inList     = regexp.MustCompile(`(?is)([a-z_][a-z0-9_]*)\s+(?:NOT\s+)?IN\s*\(([^)]*)\)`)
	// eqLiteral catches `col = 'lit'` and `col <> 'lit'` inside a RULE. A
	// vocabulary CHECK is not the only place a literal reaches a column:
	// 0004's biconditional binds disposition_detail with `=`, not with IN, and
	// a typo there would run, error on nothing, and silently stop binding.
	eqLiteral  = regexp.MustCompile(`(?is)([a-z_][a-z0-9_]*)\s*(?:=|<>|!=)\s*'((?:[^']|'')*)'`)
	strLiteral = regexp.MustCompile(`^'((?:[^']|'')*)'$`)
)

// unwrapParens strips ONE fully-enclosing pair of parentheses, and only when it
// really encloses the whole expression: `(a) AND (b)` is left alone.
func unwrapParens(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '(' || s[len(s)-1] != ')' {
		return s
	}
	depth, inStr := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case inStr:
			if s[i] == '\'' {
				inStr = false
			}
		case s[i] == '\'':
			inStr = true
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return s
			}
		}
	}
	if depth != 0 {
		return s
	}
	return strings.TrimSpace(s[1 : len(s)-1])
}

// stripSQLComments removes -- and /* */ comments, respecting single-quoted
// string literals (in which ” is an escaped quote). sqlite_master hands back
// the CREATE text verbatim, and these migrations carry more prose than SQL.
func stripSQLComments(s string) string {
	var b []rune
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			b = append(b, c)
			for i++; i < len(rs); i++ {
				b = append(b, rs[i])
				if rs[i] == '\'' {
					if i+1 < len(rs) && rs[i+1] == '\'' {
						i++
						b = append(b, rs[i])
						continue
					}
					break
				}
			}
		case c == '-' && i+1 < len(rs) && rs[i+1] == '-':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			b = append(b, '\n')
		case c == '/' && i+1 < len(rs) && rs[i+1] == '*':
			i += 2
			for i+1 < len(rs) && !(rs[i] == '*' && rs[i+1] == '/') {
				i++
			}
			i++
			b = append(b, ' ')
		default:
			b = append(b, c)
		}
	}
	return string(b)
}

// checkBodies returns every CHECK expression in a CREATE statement, with the
// outer parentheses removed and whitespace collapsed.
func checkBodies(ddl string) []string {
	var out []string
	s := stripSQLComments(ddl)
	upper := strings.ToUpper(s)
	for i := 0; i+5 <= len(s); i++ {
		if upper[i:i+5] != "CHECK" {
			continue
		}
		// A word boundary on both sides, so `CHECKSUM` is not a CHECK.
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
		depth, k, inStr := 0, j, false
		for ; k < len(s); k++ {
			switch {
			case inStr:
				if s[k] == '\'' {
					inStr = false
				}
			case s[k] == '\'':
				inStr = true
			case s[k] == '(':
				depth++
			case s[k] == ')':
				depth--
			}
			if depth == 0 && !inStr && s[k] == ')' {
				break
			}
		}
		if k >= len(s) {
			continue
		}
		out = append(out, strings.Join(strings.Fields(s[j+1:k]), " "))
		i = k
	}
	return out
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// literalList parses an IN list. ok is false when any item is not a single
// quoted string — an integer domain such as IN (0, 1) is a boolean column, not
// a closed vocabulary, and internal/exec's closed sets are all ~string.
func literalList(items string) ([]string, bool) {
	var out []string
	for _, raw := range strings.Split(items, ",") {
		m := strLiteral.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			return nil, false
		}
		out = append(out, strings.ReplaceAll(m[1], "''", "'"))
	}
	sort.Strings(out)
	return out, true
}

// guardedVocab is a vocabulary that binds a column only under a condition.
type guardedVocab struct {
	guard   string
	members []string
}

// schemaColumnVocabularies reads the live database and returns, per
// "table.column": the literal set an UNCONDITIONAL CHECK admits, the set a
// GUARDED CHECK admits together with its guard, and every literal found in a
// rule-shaped CHECK — so nothing containing literals goes unexamined.
//
// It is two passes on purpose. Whether `x <> 'a' OR y IN (...)` is a guarded
// vocabulary for y or a rule about y depends on whether y has an unconditional
// vocabulary of its own, and CHECKs arrive in schema order, not in that one.
func schemaColumnVocabularies(t *testing.T, db *DB) (map[string][]string, map[string][][]string) {
	vocab, _, rules := schemaColumnVocabulariesFull(t, db)
	return vocab, rules
}

func schemaColumnVocabulariesFull(t *testing.T, db *DB) (vocab map[string][]string, guarded map[string]guardedVocab, rules map[string][][]string) {
	t.Helper()
	vocab = map[string][]string{}
	guarded = map[string]guardedVocab{}
	rules = map[string][][]string{}

	type checkBody struct{ table, body string }
	var bodies []checkBody

	rows, err := db.reader.QueryContext(context.Background(),
		`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND sql IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, ddl string
		if err := rows.Scan(&table, &ddl); err != nil {
			t.Fatal(err)
		}
		for _, body := range checkBodies(ddl) {
			bodies = append(bodies, checkBody{table, body})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// PASS 1 — the unconditional vocabularies.
	var rest []checkBody
	for _, cb := range bodies {
		expr := cb.body
		// `col IS NULL OR <rest>` is the nullable spelling of the same
		// vocabulary; peel it only when the same column is named twice.
		var nullable string
		if m := nullShape.FindStringSubmatch(expr); m != nil {
			nullable, expr = m[1], m[2]
		}
		m := vocabShape.FindStringSubmatch(expr)
		if m == nil || (nullable != "" && !strings.EqualFold(nullable, m[1])) {
			rest = append(rest, cb)
			continue
		}
		members, ok := literalList(m[2])
		if !ok {
			// An integer domain. Nothing to compare against exec.
			continue
		}
		key := cb.table + "." + strings.ToLower(m[1])
		if prev, seen := vocab[key]; seen {
			t.Errorf("%s has two vocabulary CHECKs: %v and %v", key, prev, members)
		}
		vocab[key] = members
	}

	// PASS 2 — guarded vocabularies, then rules.
	var ruleBodies []checkBody
	for _, cb := range rest {
		gm := guardShape.FindStringSubmatch(cb.body)
		if gm == nil {
			ruleBodies = append(ruleBodies, cb)
			continue
		}
		expr := unwrapParens(gm[2])
		var nonNull string
		if m := notNull.FindStringSubmatch(expr); m != nil {
			nonNull, expr = m[1], m[2]
		}
		m := vocabShape.FindStringSubmatch(expr)
		if m == nil || (nonNull != "" && !strings.EqualFold(nonNull, m[1])) {
			ruleBodies = append(ruleBodies, cb)
			continue
		}
		members, ok := literalList(m[2])
		key := cb.table + "." + strings.ToLower(m[1])
		// A column with an unconditional vocabulary keeps it: this body is a
		// RULE about that column and is checked as a subset below.
		if _, unconditional := vocab[key]; !ok || unconditional {
			ruleBodies = append(ruleBodies, cb)
			continue
		}
		if nonNull == "" {
			t.Errorf("%s has a guarded vocabulary CHECK that does not exclude NULL: %q.\n"+
				"`col IN (...)` evaluates to NULL when col is NULL and SQLite PASSES a CHECK that "+
				"evaluates to NULL, so the guarded set does not bind on the absent value. Spell it "+
				"`<guard> OR (col IS NOT NULL AND col IN (...))`.", key, cb.body)
		}
		if prev, seen := guarded[key]; seen {
			t.Errorf("%s has two guarded vocabulary CHECKs: %v and %v", key, prev.members, members)
		}
		guarded[key] = guardedVocab{guard: strings.Join(strings.Fields(gm[1]), " "), members: members}
	}

	for _, cb := range ruleBodies {
		// Not a vocabulary: a rule. Every literal IN list inside it is still
		// collected, so a typo in `outcome IN ('completed', 'deadline-exceeded')`
		// cannot hide behind the shape.
		for _, m := range inList.FindAllStringSubmatch(cb.body, -1) {
			if members, ok := literalList(m[2]); ok {
				key := cb.table + "." + strings.ToLower(m[1])
				rules[key] = append(rules[key], members)
			}
		}
	}

	// And every `col = 'lit'` / `col <> 'lit'` in ANY check body, for the
	// columns that have a vocabulary at all. This is where 0004's biconditional
	// is bound: it names 'github_internal_workflow' with `=`, not with IN, and
	// nothing above would have looked at it.
	//
	// Scoped to columns with a vocabulary on purpose. `timezone <> ''` and
	// `selector_rev <> ''` are non-emptiness rules on free-text columns; there
	// is no set for them to be a member of, and demanding one would turn every
	// such rule into a false failure.
	for _, cb := range bodies {
		for _, m := range eqLiteral.FindAllStringSubmatch(cb.body, -1) {
			key := cb.table + "." + strings.ToLower(m[1])
			set, ok := vocab[key]
			if !ok {
				if g, isGuarded := guarded[key]; isGuarded {
					set, ok = g.members, true
				}
			}
			if !ok {
				continue
			}
			lit := strings.ReplaceAll(m[2], "''", "'")
			if !contains(set, lit) {
				t.Errorf("a CHECK on %s compares it with %q, which is not in the column's own vocabulary %v: %q",
					key, lit, set, cb.body)
			}
		}
	}

	return vocab, guarded, rules
}

// TestSchemaCheckLiteralsAreDerived is the mechanism the storage layer described
// and deliberately did not build.
func TestSchemaCheckLiteralsAreDerived(t *testing.T) {
	db := newLedger(t)
	vocab, guarded, rules := schemaColumnVocabulariesFull(t, db)
	want := schemaVocabularies()

	if len(vocab) == 0 {
		t.Fatal("no closed-vocabulary CHECK was found in the schema at all; the scanner is broken and every assertion below is vacuous")
	}
	if len(guarded) == 0 {
		t.Fatal("no GUARDED vocabulary CHECK was found in the schema at all; 0004 added one (drill_candidate.disposition_detail) and the scanner is no longer seeing it")
	}

	// 1. Every closed vocabulary in the SCHEMA must have a Go source of truth.
	//    This is the direction that catches an eighth closed set: a new CHECK
	//    with no entry here fails, it does not pass quietly.
	for _, key := range sortedKeysOf(vocab) {
		src, ok := want[key]
		if !ok {
			t.Errorf("%s has a closed CHECK vocabulary %v that no Go set is compared against.\n"+
				"Add it to schemaVocabularies(): derived from internal/exec if it transcribes one of the seven closed sets, "+
				"or with an explicit exemption saying why it is shadow's own.", key, vocab[key])
			continue
		}
		if src.guard != "" {
			t.Errorf("%s is registered as a GUARDED vocabulary (%s) and the schema closes it UNCONDITIONALLY. "+
				"The guard is part of the contract: either the schema lost it or the registry invented it.", key, src.guard)
		}
		if !equalSets(vocab[key], src.members) {
			msg := fmt.Sprintf("%s: the schema admits %v, Go holds %v", key, vocab[key], src.members)
			if src.exemption != "" {
				msg += "\n  (this column is EXEMPT from derivation: " + src.exemption + ")"
			}
			t.Error(msg)
		}
	}

	// 1b. Same direction for the guarded ones, plus the guard itself. A
	//     vocabulary that binds a different set of rows than the registry says
	//     is a vocabulary nobody is checking.
	for _, key := range sortedKeysOf(guarded) {
		src, ok := want[key]
		if !ok {
			t.Errorf("%s has a GUARDED CHECK vocabulary %v (under %s) that no Go set is compared against.\n"+
				"Add it to schemaVocabularies() with its guard.", key, guarded[key].members, guarded[key].guard)
			continue
		}
		if src.guard == "" {
			t.Errorf("%s is closed under the guard %q and schemaVocabularies() registers it as unconditional. "+
				"A column closed only under a condition is free text everywhere else, and the registry has to say so.",
				key, guarded[key].guard)
		} else if !strings.EqualFold(src.guard, guarded[key].guard) {
			t.Errorf("%s: the schema guards the vocabulary with %q and Go expects %q — the same members bound to a DIFFERENT set of rows",
				key, guarded[key].guard, src.guard)
		}
		if !equalSets(guarded[key].members, src.members) {
			msg := fmt.Sprintf("%s: the schema admits %v under %s, Go holds %v", key, guarded[key].members, guarded[key].guard, src.members)
			if src.exemption != "" {
				msg += "\n  (this column is EXEMPT from derivation: " + src.exemption + ")"
			}
			t.Error(msg)
		}
	}

	// 2. And every Go set here must still have a column. A stale entry is a
	//    guard pointed at nothing, which reads exactly like a guard.
	for _, key := range sortedKeysOf(want) {
		_, unconditional := vocab[key]
		_, conditional := guarded[key]
		if !unconditional && !conditional {
			t.Errorf("schemaVocabularies() names %s, which the schema no longer has a closed CHECK for", key)
		}
	}

	// 3. Rule-shaped IN lists are not vocabularies, but every literal in them
	//    must still be a member of the column's vocabulary. `outcome IN
	//    ('completed', 'deadline_exceeded')` is a rule; a typo inside it runs,
	//    errors on nothing, and silently stops binding.
	for _, key := range sortedKeysOf(rules) {
		members, ok := vocab[key]
		if !ok {
			if g, isGuarded := guarded[key]; isGuarded {
				members, ok = g.members, true
			}
		}
		if !ok {
			t.Errorf("a CHECK rule names literals on %s, which has no vocabulary CHECK at all: %v", key, rules[key])
			continue
		}
		for _, list := range rules[key] {
			for _, v := range list {
				if !contains(members, v) {
					t.Errorf("a CHECK rule on %s names %q, which is not in the column's own vocabulary %v", key, v, members)
				}
			}
		}
	}
}

// TestExemptVocabulariesAgreeWithTheirGoValidators binds the four columns that
// are NOT derived from internal/exec to the Go predicates that are their source
// of truth, so an exemption cannot become a place where nothing is checked.
func TestExemptVocabulariesAgreeWithTheirGoValidators(t *testing.T) {
	db := newLedger(t)
	vocab, guarded, _ := schemaColumnVocabulariesFull(t, db)

	for _, v := range vocab["drill_day.status"] {
		if !DayStatus(v).Valid() {
			t.Errorf("drill_day.status admits %q and shadow.DayStatus.Valid rejects it", v)
		}
	}
	for _, v := range vocab["drill_day.drop_reason"] {
		if !DropReason(v).Valid() {
			t.Errorf("drill_day.drop_reason admits %q and shadow.DropReason.Valid rejects it", v)
		}
	}
	for _, v := range vocab["exec_job_result.source"] {
		if !JobSource(v).Valid() {
			t.Errorf("exec_job_result.source admits %q and shadow.JobSource.Valid rejects it", v)
		}
	}
	for _, v := range vocab["drill_candidate.source"] {
		if !CandidateSource(v).Valid() {
			t.Errorf("drill_candidate.source admits %q and shadow.CandidateSource.Valid rejects it", v)
		}
	}
	for _, v := range vocab["drill_candidate.disposition"] {
		if !Disposition(v).Valid() {
			t.Errorf("drill_candidate.disposition admits %q and shadow.Disposition.Valid rejects it", v)
		}
	}
	for _, v := range guarded["drill_candidate.disposition_detail"].members {
		if !OutOfScopeDetail(v).Valid() {
			t.Errorf("drill_candidate.disposition_detail admits %q under out_of_scope and shadow.OutOfScopeDetail.Valid rejects it", v)
		}
	}

	// AND THE SET IS PINNED MEMBER BY MEMBER, which no other vocabulary here
	// is. The drill design puts the whole weight of "excluding by
	// not-selecting is forbidden" on what this set does NOT contain: nothing in
	// it can say "we cannot reproduce it", "the inputs are missing", "act does
	// not support this" or "it mismatched last time". A run we cannot replay
	// therefore has to be recorded, attempted, refused by the executor and paid
	// out of the gate's own 5%.
	//
	// Derivation alone would not hold that. shadow.Dispositions() derives the
	// CHECK, so adding an eighth constant would keep both sides agreeing and
	// this test green — the rule would come undone with nothing to see. Naming
	// the seven means the eighth has to be typed here as well, in a test whose
	// comment says what it costs.
	wantDispositions := []string{
		"already_selected", "deferred", "not_concluded", "not_created",
		"out_of_scope", "selected", "undecidable",
	}
	if !equalSets(vocab["drill_candidate.disposition"], wantDispositions) {
		t.Errorf("drill_candidate.disposition is %v, want exactly %v.\n"+
			"If a member was ADDED: the drill design closes this set at seven and the eighth is a "+
			"reviewed design change, not a convenience. Check first that the new value does "+
			"not name OUR inability to run something — that class must become a candidate and land in X.",
			vocab["drill_candidate.disposition"], wantDispositions)
	}
	for _, v := range vocab["exec_step_observation.conclusion"] {
		if !ValidStepConclusion(exec.Conclusion(v)) {
			t.Errorf("exec_step_observation.conclusion admits %q and shadow.ValidStepConclusion rejects it", v)
		}
	}
	// The step column is NARROWER than the job column, and that is a decision.
	// If the two ever coincide, either the schema's narrowing was dropped or the job
	// column lost values — both are changes that must be seen.
	job, step := vocab["exec_job_result.conclusion"], vocab["exec_step_observation.conclusion"]
	if equalSets(job, step) {
		t.Errorf("the step and job conclusion vocabularies have become identical (%v); the step-level narrowing is a decision, not an accident", step)
	}
	for _, v := range step {
		if !contains(job, v) {
			t.Errorf("exec_step_observation.conclusion admits %q, which the job column does not; the narrowing may only remove values", v)
		}
	}
	// Residue kinds have no typed carrier, so Verdict.Validate is the Go side.
	for _, v := range vocab["exec_residue.kind"] {
		err := Verdict{
			InvocationID:  "v",
			Outcome:       exec.OutcomeRefused,
			RefusalReason: exec.RefusalTokenAbsent,
			Residue:       []Residue{{Kind: v, Ref: "r"}},
		}.Validate()
		if err != nil {
			t.Errorf("exec_residue.kind admits %q and Verdict.Validate rejects it: %v", v, err)
		}
	}
}

// TestExecClosedSetsCanAllListThemselves closes the exec package's own residual: the
// CHECK-literal test above derives the sets it KNOWS about, so a closed set
// added to internal/exec without an enumerator reintroduces the hole silently.
// This finds the type instead of trusting the list.
func TestExecClosedSetsCanAllListThemselves(t *testing.T) {
	// Named string types in internal/exec that carry a Valid() bool method are
	// closed sets by construction. Each one must be enumerable.
	for _, tc := range []struct {
		name    string
		members []string
		valid   func(string) bool
	}{
		{"Outcome", strs(exec.Outcomes()), func(s string) bool { return exec.Outcome(s).Valid() }},
		{"RefusalReason", strs(exec.RefusalReasons()), func(s string) bool { return exec.RefusalReason(s).Valid() }},
		{"Conclusion", strs(exec.Conclusions()), func(s string) bool { return exec.Conclusion(s).Valid() }},
		{"RunConclusion", strs(exec.RunConclusions()), func(s string) bool { return exec.Conclusion(s).IsGitHub() }},
		{"SupplyMode", strs(exec.SupplyModes()), func(s string) bool { return exec.SupplyMode(s).Valid() }},
		{"Purpose", strs(exec.Purposes()), func(s string) bool { return exec.Purpose(s).Valid() }},
		{"Comparison", strs(exec.Comparisons()), func(s string) bool { return exec.Comparison(s).Valid() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.members) == 0 {
				t.Fatal("the enumerator returned nothing")
			}
			// The enumerator and Valid() must read the same membership: a
			// slice that lists members the type's own Valid() does not consult
			// is a third copy, not a derivation.
			for _, m := range tc.members {
				if !tc.valid(m) {
					t.Errorf("%s() lists %q and Valid() rejects it", tc.name, m)
				}
			}
			if !sort.StringsAreSorted(tc.members) {
				t.Errorf("%s() is not sorted: %v", tc.name, tc.members)
			}
			if tc.valid("") || tc.valid("no_such_member") {
				t.Errorf("%s.Valid() admits a value outside the set", tc.name)
			}
		})
	}
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
