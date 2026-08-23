package shadow

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// The retention rules, retained as tests.
//
// The defect behind them was DRIVEN, not imagined: PurgeDays deleted a childless
// `ran` day — the second audit query's only subject — with no FK violation,
// no error and no warning, and the audit went green afterwards. Every proof of
// the repair lived in one subagent transcript and re-ran nowhere.
//
// Each test below that guards a rule also drives the defect the rule exists
// for, in the same file, against the unexported purge the rule took the caller's
// access to. A test that cannot fail against the broken code proves nothing,
// and the broken code here is still one function call away.

// ledgerWithRecentMax gives every retention test a MAX(day) far enough ahead
// that the window rule is not what is under test. 2026-03-01 makes the gate's
// window start at 2026-02-09, so any cutoff in January is legal.
const (
	maxDay        = "2026-03-01"
	maxDayRunID   = "run-max"
	januaryCutoff = "2026-01-20"
)

func ledgerWithRecentMax(t *testing.T) *DB {
	t.Helper()
	db := newLedger(t)
	mustDay(t, db, maxDay, DayRan, "")
	mustDrillRun(t, db, invSpec{id: maxDayRunID, day: maxDay})
	return db
}

// --------------------------------------------------------------------------
// Rule 1 — if either audit returns rows, NOTHING is deleted.
// --------------------------------------------------------------------------

// TestRetainDeletesNothingWhenAudit2Fires is the single-call-site rule's own subject: a `ran` day
// with no drill run is the cheapest thing in the ledger to delete, and deleting
// it repairs the EVIDENCE rather than the defect.
func TestRetainDeletesNothingWhenAudit2Fires(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)

	// The defective day: `ran`, before the cutoff, and childless.
	mustDay(t, db, "2026-01-05", DayRan, "")

	before := countRows(t, db)

	res, err := db.Retain(ctx, januaryCutoff)
	if err == nil {
		t.Fatalf("Retain returned %+v and no error; the audit had to refuse it", res)
	}
	var refused *RetentionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("want *RetentionRefused, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrRetentionAuditFired) {
		t.Fatalf("errors.Is(ErrRetentionAuditFired) is false: %v", err)
	}
	// Rule 1 requires the offending ROWS, not a count.
	if got := refused.RanDaysWithoutDrillRuns; len(got) != 1 || got[0] != "2026-01-05" {
		t.Fatalf("RanDaysWithoutDrillRuns = %v, want [2026-01-05]", got)
	}
	if !strings.Contains(err.Error(), "2026-01-05") {
		t.Fatalf("the refusal does not name the day: %s", err)
	}
	if res != (RetainResult{}) {
		t.Fatalf("a refusal reported deletions: %+v", res)
	}
	if after := countRows(t, db); after != before {
		t.Fatalf("rows changed across a refusal:\n before %+v\n after  %+v", before, after)
	}
	if !dayExists(t, db, "2026-01-05") {
		t.Fatal("the day the audit named was deleted by the call that refused")
	}

	// The other half of the proof: the delete the rule protects that day from
	// is still capable of taking it. Without this the test above could be
	// green because nothing could have deleted the day in the first place.
	t.Run("the purge alone still takes it", func(t *testing.T) {
		var n int64
		err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
			var err error
			n, err = purgeDays(ctx, tx, januaryCutoff)
			return err
		})
		if err != nil {
			t.Fatalf("purgeDays: %v", err)
		}
		if n != 1 {
			t.Fatalf("purgeDays deleted %d rows, want 1 — the audit's subject is no longer reachable by the delete, so the guard above proves nothing", n)
		}
		if dayExists(t, db, "2026-01-05") {
			t.Fatal("purgeDays reported a deletion that did not happen")
		}
		// And now the audit is green, which is the defect in one sentence.
		silent, err := db.RanDaysWithoutDrillRuns(ctx)
		if err != nil {
			t.Fatalf("audit 2: %v", err)
		}
		if len(silent) != 0 {
			t.Fatalf("audit 2 still fires after the delete: %v", silent)
		}
	})
}

// TestRetainDeletesNothingWhenAudit1Fires is the first audit's half: a drill
// run written to a day the ledger says was dropped.
func TestRetainDeletesNothingWhenAudit1Fires(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)

	mustDay(t, db, "2026-01-05", DayDropped, DropGitHubUnreachable)
	mustDrillRun(t, db, invSpec{id: "run-on-dropped-day", day: "2026-01-05"})

	before := countRows(t, db)

	_, err := db.Retain(ctx, januaryCutoff)
	var refused *RetentionRefused
	if !errors.As(err, &refused) {
		t.Fatalf("want *RetentionRefused, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrRetentionAuditFired) {
		t.Fatalf("errors.Is(ErrRetentionAuditFired) is false: %v", err)
	}
	if len(refused.DrillRunsOnDroppedDays) != 1 {
		t.Fatalf("DrillRunsOnDroppedDays = %+v, want one row", refused.DrillRunsOnDroppedDays)
	}
	got := refused.DrillRunsOnDroppedDays[0]
	if got.InvocationID != "run-on-dropped-day" || got.Day != "2026-01-05" || got.DropReason != DropGitHubUnreachable {
		t.Fatalf("audit 1 row = %+v, want the run, its day and its drop reason", got)
	}
	if after := countRows(t, db); after != before {
		t.Fatalf("rows changed across a refusal:\n before %+v\n after  %+v", before, after)
	}

	// An OUTAGE run on a dropped day is NOT a defect — it is the product
	// working, and audit 1's `purpose = 'drill'` filter is the whole reason.
	t.Run("an outage run on the same day does not fire it", func(t *testing.T) {
		db := ledgerWithRecentMax(t)
		mustDay(t, db, "2026-01-05", DayDropped, DropGitHubUnreachable)
		mustOpen(t, db, invSpec{id: "outage-run", day: "2026-01-05", purpose: exec.PurposeOutage})
		rows, err := db.DrillRunsOnDroppedDays(ctx)
		if err != nil {
			t.Fatalf("audit 1: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("audit 1 fired on an outage run: %+v", rows)
		}
		if _, err := db.Retain(ctx, januaryCutoff); err != nil {
			t.Fatalf("Retain refused a sound ledger: %v", err)
		}
	})
}

// TestRetainRefusalCarriesEveryCause pins Unwrap() []error: when the audit and
// the window rule fire together, errors.Is must answer for each separately.
func TestRetainRefusalCarriesEveryCause(t *testing.T) {
	ctx := context.Background()
	db := newLedger(t)
	// MAX(day) is 2026-01-10, so the window starts at 2025-12-22 and a cutoff
	// of 2026-01-05 is inside it. The childless `ran` day fires audit 2.
	mustDay(t, db, "2026-01-10", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "r1", day: "2026-01-10"})
	mustDay(t, db, "2026-01-02", DayRan, "")

	err := func() error { _, e := db.Retain(ctx, "2026-01-05"); return e }()
	if !errors.Is(err, ErrRetentionAuditFired) {
		t.Fatalf("ErrRetentionAuditFired not reported: %v", err)
	}
	if !errors.Is(err, ErrCutoffInsideWindow) {
		t.Fatalf("ErrCutoffInsideWindow not reported: %v", err)
	}
	var refused *RetentionRefused
	errors.As(err, &refused)
	if len(refused.Causes) != 2 {
		t.Fatalf("Causes = %v, want both rules", refused.Causes)
	}
	// The rules' own order: the audit rule first, the window rule second.
	if refused.Causes[0] != ErrRetentionAuditFired || refused.Causes[1] != ErrCutoffInsideWindow {
		t.Fatalf("Causes are out of rule order: %v", refused.Causes)
	}
}

// --------------------------------------------------------------------------
// Rule 2 — the horizon is DERIVED from the ledger, and no clock is read.
// --------------------------------------------------------------------------

// TestRetainWindowBoundary is the measured boundary: on a ledger whose MAX(day)
// is 2026-08-23, a cutoff 20 days back is refused and 21 days back runs — a
// retained span of 22 days, rule 2's "strictly longer than the window" with the
// boundary on the safe side.
func TestRetainWindowBoundary(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		cutoff  string
		refused bool
	}{
		{"one day inside the window", "2026-08-04", true},
		{"the first day of the window", "2026-08-03", true},
		{"one day before the window", "2026-08-02", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newLedger(t)
			mustDay(t, db, "2026-08-23", DayRan, "")
			mustDrillRun(t, db, invSpec{id: "newest", day: "2026-08-23"})

			res, err := db.Retain(ctx, tc.cutoff)
			if tc.refused {
				if !errors.Is(err, ErrCutoffInsideWindow) {
					t.Fatalf("cutoff %s: want ErrCutoffInsideWindow, got %v", tc.cutoff, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cutoff %s: %v", tc.cutoff, err)
			}
			if res.WindowStart != "2026-08-03" || res.LatestDay != "2026-08-23" {
				t.Fatalf("derived window = [%s .. %s], want [2026-08-03 .. 2026-08-23]", res.WindowStart, res.LatestDay)
			}
		})
	}
}

// TestRetainRefusesAnUndeterminableWindow covers both directions the derivation
// can fail in, and both fail CLOSED.
func TestRetainRefusesAnUndeterminableWindow(t *testing.T) {
	ctx := context.Background()

	t.Run("empty ledger", func(t *testing.T) {
		db := newLedger(t)
		if _, err := db.Retain(ctx, "2020-01-01"); !errors.Is(err, ErrWindowUndeterminable) {
			t.Fatalf("want ErrWindowUndeterminable, got %v", err)
		}
	})

	// The column's GLOB checks the SHAPE, not the date, so 9999-99-99 is a
	// storable day key that becomes MAX(day) by string order. date() returns
	// NULL for it, and retention wedges until a human removes it — deliberately
	// chosen over deleting against a window nobody could compute.
	t.Run("MAX(day) is shaped like a date and is not one", func(t *testing.T) {
		db := newLedger(t)
		mustDay(t, db, "2026-08-23", DayRan, "")
		mustDrillRun(t, db, invSpec{id: "newest", day: "2026-08-23"})
		mustDay(t, db, "9999-99-99", DayDropped, DropNoCandidateRun)

		if _, err := db.Retain(ctx, "2026-08-02"); !errors.Is(err, ErrWindowUndeterminable) {
			t.Fatalf("want ErrWindowUndeterminable, got %v", err)
		}
	})
}

// TestRetainReadsNoClock holds the derivation mechanically, from two sides.
//
// The first is behavioural: Config.Now is the package's only injected clock and
// Retain must not touch it. The second is structural, because a direct
// time.Now() would slip past the first — retention.go may not import "time" at
// all.
func TestRetainReadsNoClock(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "old", day: "2026-01-05"})

	db.cfg.Now = func() time.Time {
		t.Error("Retain read the injected wall clock; the horizon must come from MAX(day)")
		return time.Now()
	}
	if _, err := db.Retain(ctx, januaryCutoff); err != nil {
		t.Fatalf("Retain: %v", err)
	}

	t.Run("retention.go imports no clock", func(t *testing.T) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "retention.go", nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing retention.go: %v", err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"time"` {
				t.Fatal(`retention.go imports "time"; the retention horizon is derived from the ledger and must read no clock`)
			}
		}
	})
}

// --------------------------------------------------------------------------
// The cutoff is a DAY KEY and both axes are purged on it.
// --------------------------------------------------------------------------

// TestPurgeBoundsOnDrillDayNotStartedAt constructs the skew the two axes can
// carry and shows which one decides.
//
// A wall-clock horizon needs the drill's timezone, which this package
// deliberately does not own, and any skew between the axes lets retention take
// a run out of a day the gate is still counting. The survivor below is exactly
// that run: its drill_day is inside the retained span and its started_at is
// four months before the cutoff.
func TestPurgeBoundsOnDrillDayNotStartedAt(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)

	mustDay(t, db, "2026-01-05", DayRan, "") // before the cutoff
	mustDay(t, db, "2026-02-20", DayRan, "") // after the cutoff, inside no rule

	// Skew one way: an old DAY stamped with a new clock. Bounded on drill_day,
	// it goes; bounded on started_at, it would have stayed.
	mustDrillRun(t, db, invSpec{
		id: "old-day-new-clock", day: "2026-01-05",
		startedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	})
	// Skew the other way: a day the gate is still counting, stamped with a
	// clock from before the cutoff. THIS is the row rule 2 exists for.
	mustDrillRun(t, db, invSpec{
		id: "new-day-old-clock", day: "2026-02-20",
		startedAt: time.Date(2025, 10, 1, 12, 0, 0, 0, time.UTC),
	})

	res, err := db.Retain(ctx, "2026-02-01")
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if invocationExists(t, db, "new-day-old-clock") == false {
		t.Fatal("a run on a day INSIDE the retained span was deleted; the purge is bounding on started_at, and the gate's denominator just lost a run it is still counting")
	}
	if invocationExists(t, db, "old-day-new-clock") {
		t.Fatal("a run on a day BEFORE the cutoff survived; the purge is bounding on started_at")
	}
	if res.Invocations != 1 {
		t.Fatalf("Invocations = %d, want 1", res.Invocations)
	}
	if res.Days != 1 || dayExists(t, db, "2026-01-05") {
		t.Fatalf("Days = %d, 2026-01-05 present = %v; the emptied day should have gone with it", res.Days, dayExists(t, db, "2026-01-05"))
	}
	if !dayExists(t, db, "2026-02-20") {
		t.Fatal("a day at or after the cutoff was deleted")
	}
}

// --------------------------------------------------------------------------
// FK order — D3's defect, driven.
// --------------------------------------------------------------------------

// TestRetainDeletesChildrenFirst shows both that Retain's order works and that
// the reverse order is REFUSED by the database rather than merely untidy.
func TestRetainDeletesChildrenFirst(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "old", day: "2026-01-05"})

	before := countRows(t, db)
	if before.Jobs == 0 || before.Steps == 0 || before.Images == 0 ||
		before.Residue == 0 || before.LocalRepos == 0 || before.Notes == 0 {
		t.Fatalf("the fixture wrote no children, so the FK order is not under test: %+v", before)
	}

	t.Run("the parent-first order is refused by the database", func(t *testing.T) {
		err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM exec_invocation WHERE drill_day < ?`, januaryCutoff)
			return err
		})
		if err == nil {
			t.Fatal("deleting exec_invocation before its children succeeded; PRAGMA foreign_keys is not ON and the delete order is holding nothing")
		}
		if code := extendedCode(err); code != constraintForeignKey {
			t.Fatalf("parent-first delete failed with extended code %d, want %d (SQLITE_CONSTRAINT_FOREIGNKEY): %v", code, constraintForeignKey, err)
		}
	})

	t.Run("deleting drill_day before its runs is refused too", func(t *testing.T) {
		err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM drill_day WHERE day = ?`, "2026-01-05")
			return err
		})
		if err == nil {
			t.Fatal("deleting a drill_day that still carries runs succeeded")
		}
		if code := extendedCode(err); code != constraintForeignKey {
			t.Fatalf("extended code %d, want %d: %v", code, constraintForeignKey, err)
		}
	})

	res, err := db.Retain(ctx, januaryCutoff)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if res.Invocations != 1 || res.Days != 1 {
		t.Fatalf("Retain deleted %d invocations and %d days, want 1 and 1", res.Invocations, res.Days)
	}
	after := countRows(t, db)
	want := rowCounts{
		Days:        before.Days - 1,
		Invocations: before.Invocations - 1,
		Jobs:        before.Jobs - 1,
		Steps:       before.Steps - 1,
		Images:      before.Images - 1,
		Residue:     before.Residue - 1,
		LocalRepos:  before.LocalRepos - 1,
		Notes:       before.Notes - 2, // phase 1 and phase 2 each wrote one
	}
	if after != want {
		t.Fatalf("children survived their parent:\n got  %+v\n want %+v", after, want)
	}

	// childTables must name every table with an FK onto exec_invocation, or
	// Retain fails at the parent delete instead of skipping quietly.
	t.Run("childTables names every child", func(t *testing.T) {
		named := map[string]bool{}
		for _, tbl := range childTables {
			named[tbl] = true
		}
		rows, err := db.reader.QueryContext(ctx, `
			SELECT m.name FROM sqlite_master m WHERE m.type = 'table'
			  AND m.name NOT LIKE 'sqlite_%' AND m.name <> 'exec_invocation'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var tables []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, n)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		for _, tbl := range tables {
			fkRows, err := db.reader.QueryContext(ctx, `SELECT "table" FROM pragma_foreign_key_list(?)`, tbl)
			if err != nil {
				t.Fatal(err)
			}
			refs := false
			for fkRows.Next() {
				var parent string
				if err := fkRows.Scan(&parent); err != nil {
					fkRows.Close()
					t.Fatal(err)
				}
				if parent == "exec_invocation" {
					refs = true
				}
			}
			fkRows.Close()
			if refs && !named[tbl] {
				t.Errorf("%s references exec_invocation and is not in childTables; Retain will fail at the parent delete", tbl)
			}
		}
	})
}

// TestPurgeDaysWillNotOrphanRuns pins the NOT EXISTS clause. It is not an
// optimisation: it is what makes the statement incapable of violating the
// foreign key regardless of the order a future caller runs it in.
func TestPurgeDaysWillNotOrphanRuns(t *testing.T) {
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "old", day: "2026-01-05"})

	var n int64
	err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		var err error
		n, err = purgeDays(ctx, tx, januaryCutoff)
		return err
	})
	if err != nil {
		t.Fatalf("purgeDays on a day that still has runs: %v", err)
	}
	if n != 0 {
		t.Fatalf("purgeDays deleted %d rows; a day carrying runs must be untouched", n)
	}
	if !dayExists(t, db, "2026-01-05") {
		t.Fatal("purgeDays deleted a day that still carries runs")
	}
}

// --------------------------------------------------------------------------
// The shape of the API is itself the mechanism.
// --------------------------------------------------------------------------

// TestRetainIsTheOnlyDeleteInThePackage derives the claim from the source
// rather than restating it. Nothing holds the audit-first ordering except the absence of
// a second way to delete, so a DELETE appearing in another file is the rule
// coming undone — and it would be invisible to every behavioural test here.
func TestRetainIsTheOnlyDeleteInThePackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == "retention.go" {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(string(src)), "DELETE FROM") {
			t.Errorf("%s contains a DELETE; retention.go is the only file allowed one", name)
		}
	}

	// And no exported entry point may reach a purge. The purges take a
	// *sql.Conn precisely so they cannot be called without a transaction
	// somebody else opened, but unexported is what makes that true.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "retention.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !fn.Name.IsExported() {
			continue
		}
		if strings.HasPrefix(fn.Name.Name, "Purge") {
			t.Errorf("retention.go exports %s; the single-call-site rule requires the purges to be unreachable from outside the package", fn.Name.Name)
		}
	}
}

// TestRetainRejectsAMalformedCutoff — a cutoff that is not a day key is a plain
// error, not a refusal, and it must never reach the string comparison the
// column's GLOB is what makes sound.
func TestRetainRejectsAMalformedCutoff(t *testing.T) {
	db := ledgerWithRecentMax(t)
	for _, cutoff := range []string{"", "2026-1-5", "2026-01-05T00:00:00Z", "yesterday"} {
		_, err := db.Retain(context.Background(), cutoff)
		if err == nil {
			t.Fatalf("Retain accepted cutoff %q", cutoff)
		}
		var refused *RetentionRefused
		if errors.As(err, &refused) {
			t.Fatalf("cutoff %q came back as a rule refusal rather than a malformed input: %v", cutoff, err)
		}
	}
}

// TestRetainUnderConcurrentCallers runs the retention path from several
// goroutines at once. The package is single-writer, so the interesting property
// is that the counts add up exactly once — a second caller must find the rows
// already gone rather than report them again.
func TestRetainUnderConcurrentCallers(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)
	for i, day := range []string{"2026-01-02", "2026-01-03", "2026-01-04"} {
		mustDay(t, db, day, DayRan, "")
		mustDrillRun(t, db, invSpec{id: "old-" + day + "-" + string(rune('a'+i)), day: day})
	}

	const callers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		totalInv  int64
		totalDays int64
		failures  []error
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := db.Retain(ctx, januaryCutoff)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			totalInv += res.Invocations
			totalDays += res.Days
		}()
	}
	wg.Wait()

	if len(failures) != 0 {
		t.Fatalf("concurrent Retain callers failed: %v", failures)
	}
	if totalInv != 3 || totalDays != 3 {
		t.Fatalf("%d callers between them deleted %d invocations and %d days, want 3 and 3 — a row was reported deleted twice", callers, totalInv, totalDays)
	}
	if c := countRows(t, db); c.Invocations != 1 || c.Days != 1 {
		t.Fatalf("after the sweep %+v remain, want only the MAX(day) row and its run", c)
	}
}

// TestRetainRollsBackWholly — a failure part way through must leave the ledger
// untouched, and the RESULT must not report the half that ran. The counts are
// only copied out after COMMIT for exactly this reason.
func TestRetainRollsBackWholly(t *testing.T) {
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "old", day: "2026-01-05"})
	before := countRows(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the transaction cannot commit

	res, err := db.Retain(ctx, januaryCutoff)
	if err == nil {
		t.Fatal("Retain committed under a cancelled context")
	}
	if res != (RetainResult{}) {
		t.Fatalf("a rolled-back pass reported deletions: %+v", res)
	}
	if after := countRows(t, db); after != before {
		t.Fatalf("rows changed across a rolled-back pass:\n before %+v\n after  %+v", before, after)
	}
}

// TestRetainReportsTheWindowItRanUnder — the third audit query is REPORTED rather
// than checked, and reporting it is what makes the numbers the purge was
// permitted under readable afterwards.
func TestRetainReportsTheWindowItRanUnder(t *testing.T) {
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "old", day: "2026-01-05"})
	mustDay(t, db, "2026-01-06", DayDropped, DropOperatorSuspended)

	res, err := db.Retain(context.Background(), januaryCutoff)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	// The window is measured BEFORE the deletes, on the same snapshot.
	want := Window{DrillDays: 2, DroppedDays: 1, WindowDays: 3}
	if res.Window != want {
		t.Fatalf("Window = %+v, want %+v — the shape must be the one the purge was permitted under, not the one left behind", res.Window, want)
	}
	if res.Cutoff != januaryCutoff || res.LatestDay != maxDay {
		t.Fatalf("result = %+v, want cutoff %s and latest day %s", res, januaryCutoff, maxDay)
	}
}

// --------------------------------------------------------------------------
// The candidate axis broke Retain, and the repair.
//
// Both failures below were DRIVEN against the pre-0003 retention order before
// the fix was written, and both came back `FOREIGN KEY constraint failed`:
//
//   - childTables did not name drill_attempt, so `DELETE FROM exec_invocation`
//     ran with attempts still pointing at it;
//   - purgeDays' NOT EXISTS looked only at exec_invocation, so a day whose
//     candidates had no invocation was deleted out from under drill_candidate.
//
// THE DIRECTION WAS RIGHT: it failed LOUDLY instead of deleting silently, which
// is the opposite of the defect the retention rules exist for. What was
// missing was the order, and childTables now gives it.
// --------------------------------------------------------------------------

// TestRetainPurgesTheCandidateAxis runs the whole shape the drill produces —
// examined candidate, both planned attempts, both invocations, linked — on a day
// before the cutoff, and a second one on the day the window protects.
func TestRetainPurgesTheCandidateAxis(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)
	mustZone(t, db, "2026-01-01", "Europe/Istanbul")

	mustDay(t, db, "2026-01-05", DayRan, "")
	mustPairedDrill(t, db, "2026-01-05", "cand-old", 1001)

	mustDay(t, db, "2026-02-20", DayRan, "")
	mustPairedDrill(t, db, "2026-02-20", "cand-new", 1002)

	before := countRows(t, db)
	if before.Candidates != 2 || before.Attempts != 4 {
		t.Fatalf("the fixture did not write the candidate axis: %+v", before)
	}

	res, err := db.Retain(ctx, "2026-02-01")
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}

	if candidateExists(t, db, "cand-old") {
		t.Fatal("a candidate on a day before the cutoff survived")
	}
	if n := attemptCount(t, db, "cand-old"); n != 0 {
		t.Fatalf("%d attempts survived their candidate", n)
	}
	if !candidateExists(t, db, "cand-new") {
		t.Fatal("a candidate on a day inside the retained span was deleted")
	}
	if n := attemptCount(t, db, "cand-new"); n != 2 {
		t.Fatalf("cand-new has %d attempts, want 2", n)
	}
	if res.Candidates != 1 || res.Attempts != 2 || res.Invocations != 2 || res.Days != 1 {
		t.Fatalf("Retain reported %+v; want 1 candidate, 2 attempts, 2 invocations, 1 day", res)
	}

	// THE ZONE ROW OUTLIVES THE DAY IT EXPLAINS. That is why it has no foreign
	// key, and it is the half a test can actually hold: without it the row
	// would either be deleted with the day or block the day's deletion.
	if c := countRows(t, db); c.Zones != 1 {
		t.Fatalf("drill_day_zone has %d rows after retention, want 1 — the record that EXPLAINS a day key was deleted with the key", c.Zones)
	}
	zones, err := db.DayZones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 1 || zones[0].SinceDay != "2026-01-01" {
		t.Fatalf("zones = %+v; the surviving day keys no longer say which timezone drew them", zones)
	}
}

// TestPurgeDaysWillNotOrphanCandidates is the second breakage on its own. A day
// before the cutoff whose candidates were all REJECTED has no invocation at all,
// so the NOT EXISTS that only consulted exec_invocation deleted it — and the
// foreign key from drill_candidate refused the statement.
//
// The clause is not an optimisation. It is what makes purgeDays incapable of
// violating a foreign key regardless of the order a future caller runs it in,
// which is the same reason the exec_invocation half is there.
func TestPurgeDaysWillNotOrphanCandidates(t *testing.T) {
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "old-run", day: "2026-01-05"})
	// A rejected candidate on a day that carries no invocation of its own.
	mustDay(t, db, "2026-01-06", DayDropped, DropNoCandidateRun)
	mustExamine(t, db, "2026-01-06", candSpec{
		id: "cand-rejected", runID: 2001,
		disposition: DispositionOutOfScope, detail: string(DetailShadowNever),
	})

	var n int64
	err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		var err error
		n, err = purgeDays(ctx, tx, januaryCutoff)
		return err
	})
	if err != nil {
		t.Fatalf("purgeDays on a day whose candidates have no invocation: %v", err)
	}
	if n != 0 {
		t.Fatalf("purgeDays deleted %d rows; a day still carrying candidates must be untouched", n)
	}
	if !dayExists(t, db, "2026-01-06") {
		t.Fatal("purgeDays deleted a day that still carries candidates")
	}
	if !candidateExists(t, db, "cand-rejected") {
		t.Fatal("the candidate went with the day")
	}
}

// TestRetainDeletesRejectedCandidatesToo closes the other half: a candidate the
// selector REJECTED is a row with no attempt and no invocation, and retention
// still has to be able to take it once its day is out of the window. If it
// could not, an old day would be undeletable forever and the ledger would grow
// without bound — which is how a retention rule gets "temporarily" disabled.
func TestRetainDeletesRejectedCandidatesToo(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)
	mustDay(t, db, "2026-01-06", DayDropped, DropNoCandidateRun)
	mustExamine(t, db, "2026-01-06",
		candSpec{id: "c-out", runID: 3001, disposition: DispositionOutOfScope, detail: string(DetailNotConfigured)},
		candSpec{id: "c-defer", runID: 3002, disposition: DispositionDeferred, detail: "daily cap reached"},
	)

	res, err := db.Retain(ctx, januaryCutoff)
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if res.Candidates != 2 || res.Attempts != 0 || res.Invocations != 0 || res.Days != 1 {
		t.Fatalf("Retain reported %+v; want 2 candidates, 0 attempts, 0 invocations, 1 day", res)
	}
	if dayExists(t, db, "2026-01-06") {
		t.Fatal("the day survived its candidates")
	}
}

// TestRetainRefusalLeavesTheCandidateAxisAlone — rule 1 covers the new
// tables too. Nothing may be deleted anywhere when an audit fires, and a
// candidate is the cheapest row in the ledger to lose quietly.
func TestRetainRefusalLeavesTheCandidateAxisAlone(t *testing.T) {
	ctx := context.Background()
	db := ledgerWithRecentMax(t)
	mustZone(t, db, "2026-01-01", "Europe/Istanbul")
	mustDay(t, db, "2026-01-05", DayRan, "")
	mustPairedDrill(t, db, "2026-01-05", "cand-old", 4001)
	// The defective day that makes audit 2 fire.
	mustDay(t, db, "2026-01-07", DayRan, "")

	before := countRows(t, db)
	if _, err := db.Retain(ctx, januaryCutoff); !errors.Is(err, ErrRetentionAuditFired) {
		t.Fatalf("want ErrRetentionAuditFired, got %v", err)
	}
	if after := countRows(t, db); after != before {
		t.Fatalf("rows changed across a refusal:\n before %+v\n after  %+v", before, after)
	}
}
