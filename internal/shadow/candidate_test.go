package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// The candidate axis's rules, its six audits and the "never read the gate
// through the join" measurement, retained as tests.
//
// The candidate axis is no longer specified as a DDL fence: it has a thirty-row
// rule table, each row naming the shipped constraint that holds it, driven against the schema
// the MIGRATIONS build rather than against a clean copy of a design document. That
// drive lived in one transcript. This file re-runs the part of it that reads
// as behaviour a caller can see.

const candDay = "2026-05-01"

// candidateLedger is a migrated ledger with one recorded day and one zone, so a
// test about the candidate axis is not also a test about the day axis.
func candidateLedger(t *testing.T) *DB {
	t.Helper()
	db := newLedger(t)
	mustZone(t, db, "2026-01-01", "Europe/Istanbul")
	mustDay(t, db, candDay, DayRan, "")
	return db
}

// --------------------------------------------------------------------------
// The rule is carried by what the closed set does NOT contain.
// --------------------------------------------------------------------------

// TestDispositionSetHasNoNameForOurOwnInability is the mechanism, stated as an
// assertion rather than a comment.
//
// The gate's rules forbid excluding by not-selecting. The
// thing that holds it is not a check anywhere in the selector — it is that the
// column cannot SPELL the excuse. A run we cannot replay has to be written down,
// attempted, refused by the executor and paid out of the gate's own 5%.
func TestDispositionSetHasNoNameForOurOwnInability(t *testing.T) {
	if got := len(Dispositions()); got != 7 {
		t.Fatalf("Dispositions() has %d members, want 7", got)
	}
	// Every member has to be a fact about GITHUB's side or about the drill's
	// own bookkeeping. None of these may become writable.
	for _, forbidden := range []string{
		"unreproducible", "cannot_replay", "unsupported", "inputs_missing",
		"event_payload_incomplete", "previously_mismatched", "act_unsupported",
		"executor_cannot_run", "too_hard",
	} {
		if Disposition(forbidden).Valid() {
			t.Errorf("disposition %q is writable; it names OUR inability, so a run in that class "+
				"could be dropped at selection instead of becoming a candidate that lands in X", forbidden)
		}
	}

	// And the Go set and the column agree, in the direction that matters here:
	// a value Go admits must be storable, or the enforcement lives in only one
	// of the two places.
	db := candidateLedger(t)
	for i, d := range Dispositions() {
		c := candSpec{
			id:          fmt.Sprintf("d-%d", i),
			day:         candDay,
			runID:       int64(9000 + i),
			disposition: d,
			detail:      "fixture",
		}
		// Two of the seven belong to the synthetic path only, and one of the
		// remaining five may not carry GitHub's conclusion. `out_of_scope`'s
		// detail is a CLOSED vocabulary since 0004, so
		// "fixture" is not writable there — which is this test's own subject
		// pointed at a different column.
		if d == DispositionNotCreated || d == DispositionUndecidable {
			c.source = SourceSynthetic
		}
		if d == DispositionNotConcluded {
			c.conclusion = ""
		}
		if d == DispositionOutOfScope {
			c.detail = string(DetailNotConfigured)
		}
		if err := db.RecordExamination(context.Background(), candDay, []Candidate{c.candidate()}); err != nil {
			t.Errorf("disposition %q is in the Go set and the schema refused it: %v", d, err)
		}
	}
}

// --------------------------------------------------------------------------
// The eleven assertions.
// --------------------------------------------------------------------------

func TestCandidateRowConstraints(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		spec   candSpec
		mutate func(*Candidate)
		accept bool
		// why names the rule, so a failure says which one came undone.
		why string
	}{
		{
			name: "a history run may not be eliminated by the prefilter",
			spec: candSpec{id: "p1", runID: 11},
			mutate: func(c *Candidate) {
				c.Disposition = DispositionNotCreated
				c.DispositionDetail = "our glob says GitHub would not have made it"
			},
			why: "GitHub CREATED that run; our filter disagreeing is a finding about the filter",
		},
		{
			name:   "a selected row needs GitHub's conclusion",
			spec:   candSpec{id: "p2", runID: 12},
			mutate: func(c *Candidate) { c.GitHubConclusion = "" },
			why:    "a run GitHub has not judged has nothing to be compared against",
		},
		{
			name: "a reasonless deferral cannot be written",
			spec: candSpec{id: "p3", runID: 13, disposition: DispositionDeferred},
			mutate: func(c *Candidate) {
				c.DispositionDetail = ""
			},
			why: "a cap that never says which cap is how a cap becomes a hiding place",
		},
		{
			name:   "a synthetic row may not carry a GitHub run id",
			spec:   candSpec{id: "p4", source: SourceSynthetic},
			mutate: func(c *Candidate) { c.GitHubRunID = 44 },
			why:    "synthetic means GitHub has no counterpart",
		},
		{
			name:   "not_concluded may not carry a conclusion",
			spec:   candSpec{id: "p5", runID: 15, disposition: DispositionNotConcluded, detail: "still running"},
			mutate: func(c *Candidate) { c.GitHubConclusion = exec.ConclusionSuccess },
			why:    "the disposition and the column say opposite things",
		},
		{
			// The name carries NO SIZE. It said "GitHub's eight" and stayed
			// saying it after `startup_failure` made the set nine — because the set is
			// exec.RunConclusions(), which internal/exec widens, and a number
			// written down here is a copy of a map maintained in another
			// package. internal/exec/vocabulary.go took the same decision for
			// its operator-facing messages after the identical drift.
			name:   "a conclusion outside GitHub's own set is refused",
			spec:   candSpec{id: "p6", runID: 16},
			mutate: func(c *Candidate) { c.GitHubConclusion = exec.ConclusionUnmappable },
			why:    "`unmappable` is OUR word for a conclusion we could not map; GitHub never writes it",
		},
		{
			name:   "a short commit sha is refused",
			spec:   candSpec{id: "p7", runID: 17},
			mutate: func(c *Candidate) { c.CommitSHA = "abc" },
			why:    "the drill's input is a SHA, and a branch name is not one",
		},
		{
			name:   "a selector with no revision is refused",
			spec:   candSpec{id: "p8", runID: 18},
			mutate: func(c *Candidate) { c.SelectorRev = "" },
			why:    "a decision with no author cannot be questioned afterwards",
		},
		{
			name:   "a well-formed selected row is accepted",
			spec:   candSpec{id: "p9", runID: 19},
			mutate: func(c *Candidate) {},
			accept: true,
		},
		{
			name:   "a well-formed synthetic row is accepted",
			spec:   candSpec{id: "p10", source: SourceSynthetic, disposition: DispositionUndecidable, detail: "glob semantics unknown"},
			mutate: func(c *Candidate) {},
			accept: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := candidateLedger(t)
			c := tc.spec.candidate()
			c.DrillDay = candDay
			tc.mutate(&c)

			err := db.RecordExamination(ctx, candDay, []Candidate{c})
			if tc.accept {
				if err != nil {
					t.Fatalf("a legal row was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the row was accepted; %s", tc.why)
			}

			// AND THE SCHEMA HOLDS IT TOO, not only Validate. A Go-side check
			// binds this package's callers; the CHECK binds anything that ever
			// opens the file. Both are asserted, because a rule held in only
			// one of the two is a rule with a way around it.
			if err := insertCandidateRaw(t, db, c); err == nil {
				t.Fatalf("Validate refused the row and the SCHEMA accepted it; %s", tc.why)
			}
		})
	}
}

// insertCandidateRaw writes a candidate with the same SQL RecordExamination
// uses but with no Go validation, so the tests above can tell the two layers
// apart.
func insertCandidateRaw(t *testing.T, db *DB, c Candidate) error {
	t.Helper()
	return inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx, `
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
			string(c.Disposition), nullString(c.DispositionDetail))
		return err
	})
}

// TestARunIsSelectedOnceInItsLifetime is the partial unique index, and it is
// what makes the promotion gate's word "distinct" true of a query that counts rows.
func TestARunIsSelectedOnceInItsLifetime(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustDay(t, db, "2026-05-02", DayRan, "")
	mustDay(t, db, "2026-05-03", DayRan, "")

	mustExamine(t, db, candDay, candSpec{id: "first", runID: 777})

	err := db.RecordExamination(ctx, "2026-05-02", []Candidate{
		candSpec{id: "second", day: "2026-05-02", runID: 777}.candidate(),
	})
	if !errors.Is(err, ErrRunAlreadySelected) {
		t.Fatalf("the same GitHub run was selected on a second day: %v", err)
	}
	if !strings.Contains(err.Error(), string(DispositionAlreadySelected)) {
		t.Fatalf("the refusal does not name the disposition the caller should write instead: %v", err)
	}

	// The legal answer: a row that RECORDS the rejection.
	mustExamine(t, db, "2026-05-02", candSpec{
		id: "second", day: "2026-05-02", runID: 777,
		disposition: DispositionAlreadySelected, detail: "selected on 2026-05-01",
	})

	// And deferral is not terminal: two deferred days, then a selection.
	mustExamine(t, db, candDay, candSpec{id: "q1", runID: 888, disposition: DispositionDeferred, detail: "daily cap"})
	mustExamine(t, db, "2026-05-02", candSpec{id: "q2", day: "2026-05-02", runID: 888, disposition: DispositionDeferred, detail: "daily cap"})
	mustExamine(t, db, "2026-05-03", candSpec{id: "q3", day: "2026-05-03", runID: 888})

	if _, err := db.Candidates(ctx, "2026-05-03"); err != nil {
		t.Fatal(err)
	}
}

// TestCandidateAxisIsAppendOnly — a decision is not rewritten. Re-examining a
// run writes a NEW row on a NEW day.
func TestCandidateAxisIsAppendOnly(t *testing.T) {
	db := candidateLedger(t)
	mustExamine(t, db, candDay, candSpec{id: "c1", runID: 21})
	mustZone(t, db, "2026-06-01", "UTC")

	for _, tc := range []struct {
		name string
		stmt string
		args []any
	}{
		{"drill_candidate", `UPDATE drill_candidate SET disposition = 'out_of_scope' WHERE candidate_id = ?`, []any{"c1"}},
		{"drill_day_zone", `UPDATE drill_day_zone SET timezone = 'UTC' WHERE since_day = ?`, []any{"2026-01-01"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
				_, err := tx.ExecContext(ctx, tc.stmt, tc.args...)
				return err
			})
			if err == nil {
				t.Fatalf("%s accepted an UPDATE", tc.name)
			}
			if code := extendedCode(err); code != constraintTrigger {
				t.Fatalf("extended code %d, want %d (SQLITE_CONSTRAINT_TRIGGER): %v", code, constraintTrigger, err)
			}
		})
	}
}

// --------------------------------------------------------------------------
// The attempt axis
// --------------------------------------------------------------------------

// TestAttemptsAreOnlyPlannedForSelectedCandidates covers both layers: the Go
// call asks first, and the trigger holds it for anything that does not.
func TestAttemptsAreOnlyPlannedForSelectedCandidates(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustExamine(t, db, candDay, candSpec{
		id: "rejected", runID: 31,
		disposition: DispositionOutOfScope, detail: string(DetailShadowNever),
	})

	if err := db.PlanAttempts(ctx, "rejected", time.UnixMilli(1_700_000_000_000)); !errors.Is(err, ErrCandidateNotSelected) {
		t.Fatalf("want ErrCandidateNotSelected, got %v", err)
	}
	if err := db.PlanAttempts(ctx, "nobody", time.UnixMilli(1_700_000_000_000)); !errors.Is(err, ErrUnknownCandidate) {
		t.Fatalf("want ErrUnknownCandidate, got %v", err)
	}

	err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO drill_attempt (candidate_id, supply_mode, planned_at) VALUES (?,?,?)`,
			"rejected", "online", 1)
		return err
	})
	if err == nil {
		t.Fatal("the schema planned an attempt for a rejected candidate")
	}
	if code := extendedCode(err); code != constraintTrigger {
		t.Fatalf("extended code %d, want %d: %v", code, constraintTrigger, err)
	}
}

// TestPlanAttemptsCannotPlanASingleMode is the single-call-site rule applied to
// the paired schedule: the call takes NO mode, so there is no way to write one.
//
// The modes come from exec.SupplyModes(), so this also fails if a mode is added
// to the executor's vocabulary and the schedule is not revisited.
func TestPlanAttemptsCannotPlanASingleMode(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustExamine(t, db, candDay, candSpec{id: "c1", runID: 41})
	if err := db.PlanAttempts(ctx, "c1", time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatalf("PlanAttempts: %v", err)
	}

	got, err := db.Attempts(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(exec.SupplyModes()) {
		t.Fatalf("PlanAttempts wrote %d attempts, want one per supply mode (%d)", len(got), len(exec.SupplyModes()))
	}
	for i, mode := range exec.SupplyModes() {
		if got[i].SupplyMode != mode || got[i].InvocationID != "" {
			t.Fatalf("attempt %d = %+v, want mode %s with no invocation yet", i, got[i], mode)
		}
	}

	// The second audit query therefore cannot fire from anything this package
	// exports — which does not make it redundant, only unreachable from here.
	unpaired, err := db.CandidatesNotPlannedInBothModes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpaired) != 0 {
		t.Fatalf("audit 2 fired on a paired plan: %+v", unpaired)
	}
}

// TestLinkInvocationIsAWriteOnceCAS — the same discipline CloseInvocation has.
func TestLinkInvocationIsAWriteOnceCAS(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustExamine(t, db, candDay, candSpec{id: "c1", runID: 51})
	if err := db.PlanAttempts(ctx, "c1", time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatal(err)
	}
	mustDrillRun(t, db, invSpec{id: "inv-a", day: candDay, mode: exec.SupplyOnline})
	mustDrillRun(t, db, invSpec{id: "inv-b", day: candDay, mode: exec.SupplyOnline})

	if err := db.LinkInvocation(ctx, "c1", exec.SupplyOnline, "inv-a"); err != nil {
		t.Fatalf("LinkInvocation: %v", err)
	}
	if err := db.LinkInvocation(ctx, "c1", exec.SupplyOnline, "inv-b"); !errors.Is(err, ErrAttemptAlreadyLinked) {
		t.Fatalf("a linked attempt was re-pointed: %v", err)
	}
	if err := db.LinkInvocation(ctx, "nobody", exec.SupplyOnline, "inv-b"); !errors.Is(err, ErrUnknownInvocation) {
		t.Fatalf("want ErrUnknownInvocation for an unknown candidate, got %v", err)
	}

	// The trigger holds the same line for a writer that is not this package.
	err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE drill_attempt SET invocation_id = ? WHERE candidate_id = ? AND supply_mode = ?`,
			"inv-b", "c1", "online")
		return err
	})
	if code := extendedCode(err); code != constraintTrigger {
		t.Fatalf("the raw UPDATE gave extended code %d, want %d: %v", code, constraintTrigger, err)
	}

	// And nothing else on the row may be rewritten at all.
	err = inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE drill_attempt SET supply_mode = 'offline' WHERE candidate_id = ? AND supply_mode = 'online'`, "c1")
		return err
	})
	if code := extendedCode(err); code != constraintTrigger {
		t.Fatalf("an attempt was re-pointed at another mode; extended code %d, want %d: %v", code, constraintTrigger, err)
	}
}

// TestLinkInvocationRefusesAContradiction holds the fourth candidate audit at the
// write. The audit still exists for writers that are not this package, and the
// next test drives it that way.
func TestLinkInvocationRefusesAContradiction(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustDay(t, db, "2026-05-02", DayRan, "")
	mustExamine(t, db, candDay, candSpec{id: "c1", runID: 61})
	if err := db.PlanAttempts(ctx, "c1", time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatal(err)
	}

	mustDrillRun(t, db, invSpec{id: "wrong-mode", day: candDay, mode: exec.SupplyOffline})
	mustDrillRun(t, db, invSpec{id: "wrong-day", day: "2026-05-02", mode: exec.SupplyOnline})
	mustOpen(t, db, invSpec{id: "wrong-purpose", day: candDay, mode: exec.SupplyOnline, purpose: exec.PurposeOutage})

	for _, id := range []string{"wrong-mode", "wrong-day", "wrong-purpose"} {
		if err := db.LinkInvocation(ctx, "c1", exec.SupplyOnline, id); !errors.Is(err, ErrAttemptContradiction) {
			t.Fatalf("%s: want ErrAttemptContradiction, got %v", id, err)
		}
	}
}

// --------------------------------------------------------------------------
// The six audits. Written, not judged.
// --------------------------------------------------------------------------

// TestCandidateAuditsAreEmptyOnASoundLedger is half the proof; each subtest
// below breaks the ledger deliberately and shows the query naming it. A check
// that never fires is not a check.
func TestCandidateAuditsAreEmptyOnASoundLedger(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustPairedDrill(t, db, candDay, "c1", 71)
	mustExamine(t, db, candDay, candSpec{
		id: "c2", runID: 72, disposition: DispositionOutOfScope, detail: string(DetailNotConfigured),
	})

	if rows, err := db.SelectedCandidatesWithoutAttempts(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("audit 1 = %+v, %v", rows, err)
	}
	if rows, err := db.CandidatesNotPlannedInBothModes(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("audit 2 = %+v, %v", rows, err)
	}
	if rows, err := db.AttemptsWithoutInvocation(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("audit 3 = %+v, %v", rows, err)
	}
	if rows, err := db.AttemptsContradictingTheirInvocation(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("audit 4 = %+v, %v", rows, err)
	}
	if rows, err := db.DrillRunsWithoutACandidate(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("audit 5 = %+v, %v", rows, err)
	}
	// The sixth is READ, not repaired, and it is empty here only because
	// nothing is queued.
	if rows, err := db.QueuedCandidatesBefore(ctx, "2026-06-01"); err != nil || len(rows) != 0 {
		t.Fatalf("audit 6 = %+v, %v", rows, err)
	}
}

func TestCandidateAudit1FiresOnAnUnplannedSelection(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustExamine(t, db, candDay, candSpec{id: "promised", runID: 81})

	rows, err := db.SelectedCandidatesWithoutAttempts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CandidateID != "promised" || rows[0].Day != candDay {
		t.Fatalf("audit 1 = %+v, want the unplanned selection", rows)
	}
	// Audit 2 sees the same candidate with ZERO modes, which is the shape the
	// LEFT JOIN exists for: an INNER JOIN would not return it at all.
	unpaired, err := db.CandidatesNotPlannedInBothModes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unpaired) != 1 || unpaired[0].Modes != 0 {
		t.Fatalf("audit 2 = %+v, want the same candidate with 0 modes", unpaired)
	}
}

func TestCandidateAudit3FiresOnAnAttemptThatNeverReachedTheLedger(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustExamine(t, db, candDay, candSpec{id: "c1", runID: 91})
	if err := db.PlanAttempts(ctx, "c1", time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatal(err)
	}
	rows, err := db.AttemptsWithoutInvocation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(exec.SupplyModes()) {
		t.Fatalf("audit 3 = %+v, want one row per planned mode — this is the ONLY gap exec_invocation's two-phase row cannot see, because there the row is never born", rows)
	}
}

func TestCandidateAudit4FiresOnAWriterThatIsNotThisPackage(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustDay(t, db, "2026-05-02", DayRan, "")
	mustExamine(t, db, candDay, candSpec{id: "c1", runID: 101})
	if err := db.PlanAttempts(ctx, "c1", time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatal(err)
	}
	// LinkInvocation refuses this; raw SQL does not, and the audit is what
	// still speaks then.
	mustDrillRun(t, db, invSpec{id: "inv-x", day: "2026-05-02", mode: exec.SupplyOffline})
	if err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE drill_attempt SET invocation_id = ? WHERE candidate_id = ? AND supply_mode = 'online'`,
			"inv-x", "c1")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.AttemptsContradictingTheirInvocation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// One row, and it names BOTH disagreements — the mode and the day — so the
	// reader does not need a second query to see the whole of it.
	if len(rows) != 1 {
		t.Fatalf("audit 4 = %+v, want one row", rows)
	}
	got := rows[0]
	if got.AttemptMode != exec.SupplyOnline || got.InvocationMode != exec.SupplyOffline ||
		got.CandidateDay != candDay || got.InvocationDay != "2026-05-02" {
		t.Fatalf("audit 4 row = %+v; it must carry both sides of both disagreements", got)
	}
}

func TestCandidateAudit6IsReadRatherThanRepaired(t *testing.T) {
	ctx := context.Background()
	db := newLedger(t)
	mustZone(t, db, "2026-01-01", "Europe/Istanbul")
	// A day on which everything examined was DEFERRED or unconcluded selected
	// nothing, so it is `dropped` / `no_candidate_run`.
	// That is the shape audit 6's rows actually come in.
	mustDay(t, db, candDay, DayDropped, DropNoCandidateRun)
	mustDay(t, db, "2026-05-02", DayDropped, DropNoCandidateRun)
	mustExamine(t, db, candDay,
		candSpec{id: "old", runID: 111, disposition: DispositionDeferred, detail: "daily cap",
			createdAt: time.UnixMilli(1_600_000_000_000)},
		candSpec{id: "new", runID: 112, disposition: DispositionNotConcluded, detail: "GitHub has not judged it",
			createdAt: time.UnixMilli(1_690_000_000_000)},
	)
	// After the watermark: still queued, but not yet worth reporting.
	mustExamine(t, db, "2026-05-02", candSpec{id: "recent", day: "2026-05-02", runID: 113,
		disposition: DispositionDeferred, detail: "daily cap"})

	rows, err := db.QueuedCandidatesBefore(ctx, "2026-05-02")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("audit 6 = %d rows, want the two before the watermark", len(rows))
	}
	// GitHub's own created order, oldest first — the deferral order, so
	// the reader sees the queue the selector will see.
	if rows[0].CandidateID != "old" || rows[1].CandidateID != "new" {
		t.Fatalf("audit 6 is not in github_created_at order: %s, %s", rows[0].CandidateID, rows[1].CandidateID)
	}
	// It is NOT a defect signal, so nothing about it refuses anything: Retain
	// runs on this ledger.
	mustDay(t, db, "2026-08-01", DayRan, "")
	mustDrillRun(t, db, invSpec{id: "newest", day: "2026-08-01"})
	if _, err := db.Retain(ctx, "2026-04-01"); err != nil {
		t.Fatalf("a queued candidate refused retention: %v", err)
	}
}

// --------------------------------------------------------------------------
// The measurement, re-run.
// --------------------------------------------------------------------------

// TestTheGateIsNeverReadThroughTheCandidateJoin re-drives the measurement that keeps the gate
// off the candidate join.
//
// A ledger with five online drill runs, one of them MISMATCHED and unclaimed by
// any candidate. Read from exec_invocation it is 4/5. Read through an INNER JOIN
// on drill_attempt the mismatch DISAPPEARS and it is 4/4 — the query runs, rows
// come back, nothing errors, and the gate passes.
//
// This test exists so that the join can never quietly become the way the gate
// asks: if DenominatorByMode ever grows one, the first number below moves to
// 100% and this fails.
func TestTheGateIsNeverReadThroughTheCandidateJoin(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)

	// Four claimed online runs: three matched, one mismatched.
	for i, comparison := range []exec.Comparison{
		exec.ComparisonMatched, exec.ComparisonMatched,
		exec.ComparisonMatched, exec.ComparisonMismatched,
	} {
		cand := fmt.Sprintf("cand-%d", i)
		inv := fmt.Sprintf("inv-%d", i)
		mustExamine(t, db, candDay, candSpec{id: cand, runID: int64(200 + i)})
		if err := db.PlanAttempts(ctx, cand, time.UnixMilli(1_700_000_000_000)); err != nil {
			t.Fatal(err)
		}
		mustDrillRun(t, db, invSpec{id: inv, day: candDay, mode: exec.SupplyOnline, comparison: comparison})
		if err := db.LinkInvocation(ctx, cand, exec.SupplyOnline, inv); err != nil {
			t.Fatal(err)
		}
	}
	// And the fifth: an unclaimed run, and it is the MISMATCHED one on purpose.
	// That is the direction that was measured, and the only direction that matters:
	// an unclaimed MATCH would make the joined reading look worse, which nobody
	// ships by accident. An unclaimed MISMATCH turns a failing gate into a
	// passing one.
	mustDrillRun(t, db, invSpec{id: "inv-unclaimed", day: candDay, mode: exec.SupplyOnline,
		comparison: exec.ComparisonMismatched})

	// Reading 1: the gate's own query.
	modes, err := db.DenominatorByMode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var online ModeDenominator
	for _, m := range modes {
		if m.SupplyMode == exec.SupplyOnline {
			online = m
		}
	}
	if online.Attempted != 5 || online.Completed != 5 || online.Matched != 3 {
		t.Fatalf("exec_invocation reading = A %d, D %d, M %d; want 5, 5, 3", online.Attempted, online.Completed, online.Matched)
	}

	// Reading 2: the same question asked through the candidate axis. This SQL
	// exists ONLY here, in a test, and it is the reason it may not exist
	// anywhere else.
	var a, d, m int
	err = db.reader.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(e.outcome = 'completed'), 0),
		       COALESCE(SUM(e.comparison = ?), 0)
		FROM exec_invocation e
		JOIN drill_attempt a ON a.invocation_id = e.invocation_id
		WHERE e.purpose = 'drill' AND e.supply_mode = 'online'`,
		string(exec.ComparisonMatched)).Scan(&a, &d, &m)
	if err != nil {
		t.Fatal(err)
	}
	if a != 4 || d != 4 || m != 3 {
		t.Fatalf("the joined reading = A %d, D %d, M %d; want 4, 4, 3 — the fixture is not the measured one", a, d, m)
	}

	// The two ratios, and the whole point in one comparison.
	honest := float64(online.Matched) / float64(online.Completed)
	joined := float64(m) / float64(d)
	if honest >= joined {
		t.Fatalf("the join did not flatter the gate (%.1f%% vs %.1f%%); the fixture no longer reproduces the measurement", honest*100, joined*100)
	}
	if honest != 0.6 || joined != 0.75 {
		t.Fatalf("ratios = %.1f%% honest, %.1f%% joined", honest*100, joined*100)
	}

	// AND THE CROSS-CHECK IS EXACTLY WHAT NAMES THE DIFFERENCE:
	// the two readings being equal is equivalent to audit 5 being empty, which
	// is why audit 5 runs BEFORE the gate's numbers are read.
	unclaimed, err := db.DrillRunsWithoutACandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unclaimed) != 1 || unclaimed[0].InvocationID != "inv-unclaimed" {
		t.Fatalf("audit 5 = %+v, want the unclaimed run — it is the ONLY thing that names the gap between the two readings", unclaimed)
	}
}

// --------------------------------------------------------------------------
// The day boundary's interpretation
// --------------------------------------------------------------------------

// TestDayZoneOutlivesTheDaysItExplains is ruling 1 of 0003, from the other
// side: the FK's absence is what lets the zone row survive, and Retain never
// deletes it.
func TestDayZoneIsAppendOnlyAndExplicit(t *testing.T) {
	ctx := context.Background()
	db := newLedger(t)

	if _, ok, err := db.LatestDayZone(ctx); err != nil || ok {
		t.Fatalf("an empty ledger reported a zone: ok=%v err=%v", ok, err)
	}
	mustZone(t, db, "2026-01-01", "Europe/Istanbul")
	if err := db.RecordDayZone(ctx, DayZone{SinceDay: "2026-01-01", Timezone: "UTC"}); !errors.Is(err, ErrZoneAlreadyRecorded) {
		t.Fatalf("a zone row was rewritten: %v", err)
	}
	// A zone CHANGE is a new row, and it is an explicit act on purpose.
	mustZone(t, db, "2026-06-01", "UTC")

	z, ok, err := db.LatestDayZone(ctx)
	if err != nil || !ok {
		t.Fatalf("LatestDayZone: %v %v", ok, err)
	}
	if z.SinceDay != "2026-06-01" || z.Timezone != "UTC" {
		t.Fatalf("LatestDayZone = %+v, want the most recent row", z)
	}

	// An offset is storable as far as the column is concerned — it only
	// requires non-empty — so the IANA rule lives in the drill's config
	// validation, NOT here. Saying so is the point: this package does not hold
	// it and must not be read as if it did.
	if err := db.RecordDayZone(ctx, DayZone{SinceDay: "2026-07-01", Timezone: "+03:00"}); err != nil {
		t.Fatalf("the column refused an offset; if that is now enforced here, the comment in candidate.go is stale: %v", err)
	}
	if err := db.RecordDayZone(ctx, DayZone{SinceDay: "2026-07-02", Timezone: ""}); err == nil {
		t.Fatal("an empty timezone was accepted; a default draws the day boundary silently")
	}
	if err := db.RecordDayZone(ctx, DayZone{SinceDay: "2026-7-3", Timezone: "UTC"}); err == nil {
		t.Fatal("a malformed since_day was accepted")
	}
}

// TestRecordExaminationIsOneDayAndOneTransaction — a scan cannot straddle a day
// boundary unnoticed, and a batch that fails writes nothing.
func TestRecordExaminationIsOneDayAndOneTransaction(t *testing.T) {
	ctx := context.Background()
	db := candidateLedger(t)
	mustDay(t, db, "2026-05-02", DayRan, "")

	err := db.RecordExamination(ctx, candDay, []Candidate{
		candSpec{id: "a", day: candDay, runID: 301}.candidate(),
		candSpec{id: "b", day: "2026-05-02", runID: 302}.candidate(),
	})
	if err == nil {
		t.Fatal("one batch wrote two days")
	}

	// A batch whose LAST row is illegal writes none of the earlier ones: the
	// rejected runs and the selected ones arrive together or not at all, which
	// is what stops a crash mid-scan from looking like a clean selection.
	err = db.RecordExamination(ctx, candDay, []Candidate{
		candSpec{id: "a", day: candDay, runID: 303}.candidate(),
		candSpec{id: "a", day: candDay, runID: 304}.candidate(),
	})
	if !errors.Is(err, ErrDuplicateCandidate) {
		t.Fatalf("want ErrDuplicateCandidate, got %v", err)
	}
	if got, err := db.Candidates(ctx, candDay); err != nil || len(got) != 0 {
		t.Fatalf("a failed batch left %d rows behind: %v", len(got), err)
	}

	// An unrecorded day is refused by the foreign key, exactly as it is on the
	// run axis: the day axis cannot acquire holes from either side.
	err = db.RecordExamination(ctx, "2026-05-09", []Candidate{
		candSpec{id: "c", day: "2026-05-09", runID: 305}.candidate(),
	})
	if !errors.Is(err, ErrUnknownDay) {
		t.Fatalf("want ErrUnknownDay, got %v", err)
	}
}

// --------------------------------------------------------------------------
// The scored path is closed to the undesigned one.
// --------------------------------------------------------------------------

// TestASyntheticCandidateCannotReachTheScore drives
// drill_candidate_synthetic_never_enters_the_score both ways.
//
// THE HOLE IT CLOSES, stated as the two constraints that produce it:
// `synthetic` FORCES github_run_id NULL and `selected` FORCES
// github_conclusion NOT NULL, so the only way to select a synthetic candidate
// was to write GitHub's verdict on a run GitHub has no id for. That field is
// what the comparison scores against, so the fabrication would flow straight
// into M — a match against a number we invented, which is worth more than a
// mismatch to whoever finds it later.
//
// It is drill_candidate_history_dispositions pointing the other way: that one
// stops a run being laundered OUT of the corpus, this one stops a verdict being
// laundered IN.
func TestASyntheticCandidateCannotReachTheScore(t *testing.T) {
	ctx := context.Background()

	// Barred: the two that reach the score, or promise to.
	for _, d := range []Disposition{DispositionSelected, DispositionDeferred} {
		t.Run("barred/"+string(d), func(t *testing.T) {
			db := candidateLedger(t)
			c := candSpec{id: "s", day: candDay, source: SourceSynthetic, disposition: d, detail: "fixture"}.candidate()

			err := db.RecordExamination(ctx, candDay, []Candidate{c})
			if err == nil {
				t.Fatalf("a synthetic candidate was %q; selecting it means inventing github_conclusion, and M is counted from that column", d)
			}
			if !strings.Contains(err.Error(), "M is counted from") {
				t.Fatalf("the refusal does not say what it protects: %v", err)
			}

			// AND THE SCHEMA HOLDS IT, not only Validate. A Go check binds this
			// package's callers; the CHECK binds anything that opens the file,
			// which is what the synthetic path will be doing.
			err = insertCandidateRaw(t, db, c)
			if err == nil {
				t.Fatalf("Validate refused %q and the SCHEMA accepted it", d)
			}
			if code := extendedCode(err); code != constraintCheck {
				t.Fatalf("extended code %d, want %d (SQLITE_CONSTRAINT_CHECK): %v", code, constraintCheck, err)
			}
			if !strings.Contains(err.Error(), "drill_candidate_synthetic_never_enters_the_score") {
				t.Fatalf("a different constraint fired, so this one is not what is holding the line: %v", err)
			}
		})
	}

	// Still writable: the five that say nothing about a score. Barring more
	// than the score needs would push the synthetic-path design toward working around the constraint
	// instead of replacing it.
	for _, d := range []Disposition{
		DispositionNotCreated, DispositionUndecidable,
		DispositionOutOfScope, DispositionAlreadySelected, DispositionNotConcluded,
	} {
		t.Run("still writable/"+string(d), func(t *testing.T) {
			db := candidateLedger(t)
			spec := candSpec{id: "s", day: candDay, source: SourceSynthetic, disposition: d, detail: "fixture"}
			if d == DispositionOutOfScope {
				spec.detail = string(DetailNotConfigured)
			}
			c := spec.candidate()
			if d == DispositionNotConcluded {
				c.GitHubConclusion = ""
			}
			if err := db.RecordExamination(ctx, candDay, []Candidate{c}); err != nil {
				t.Fatalf("synthetic + %q was refused; the bar is wider than the score needs: %v", d, err)
			}
		})
	}

	// And the bar is scoped to SYNTHETIC. The history path — the only one the
	// drill uses today — is untouched by it.
	t.Run("the history path still selects and defers", func(t *testing.T) {
		db := candidateLedger(t)
		mustExamine(t, db, candDay,
			candSpec{id: "h1", runID: 601},
			candSpec{id: "h2", runID: 602, disposition: DispositionDeferred, detail: "daily cap"},
		)
		got, err := db.Candidates(ctx, candDay)
		if err != nil || len(got) != 2 {
			t.Fatalf("Candidates = %d rows, %v", len(got), err)
		}
	})
}

// TestTheTwoRunConclusionColumnsHoldTheSameSet confirms the set the column was
// registered against, rather than assuming it.
//
// THE CENTRAL ASSERTION IS THAT THE TWO COLUMNS AGREE, and that is not
// bookkeeping. drill_candidate.github_conclusion holds GITHUB's verdict on a
// run and exec_invocation.run_conclusion holds OUR reproduction of it; the
// validator's final verdict compares them, and that comparison is where the gate's numerator M
// comes from. A value one column can hold and the other cannot makes the
// comparison ill-typed for that run — in one direction it aborts an INSERT and
// in the other it silently has no counterpart. So they are the same set by
// construction, and either of them being registered against the wrong
// enumerator has to be visible.
//
// IT WAS EIGHT AND IT IS NINE, AND THE NUMBER MOVED FOR A MEASUREMENT, NOT A
// PREFERENCE [0004, 2026-08-23]. `startup_failure` was found in
// real run data — 1 of 10,897 concluded runs in the activity-weighted frame, 7
// of 2,604 in the uniform one — and the shipped CHECKs did not admit it. A
// candidate GitHub created may not be silently dropped, so
// the omission did not cost one row: the INSERT aborted and took the day's scan
// with it.
//
// The ninth member is NOT `unmappable`, and that is the other half. The two
// sets exec exposes each have a member the other lacks: `unmappable` is OUR
// word for a conclusion we could not map, GitHub never writes it on a run, and
// a column that admitted it would record our failure as GitHub's verdict — with
// M scored against the result.
func TestTheTwoRunConclusionColumnsHoldTheSameSet(t *testing.T) {
	db := candidateLedger(t)
	vocab, _ := schemaColumnVocabularies(t, db)

	cand := vocab["drill_candidate.github_conclusion"]
	run := vocab["exec_invocation.run_conclusion"]
	if !equalSets(cand, run) {
		t.Fatalf("the candidate axis and the run axis disagree about GitHub's conclusions:\n candidate %v\n run       %v\n"+
			"They are the two sides of the comparison that produces M, so they are the same set by definition; one of them was registered against the wrong enumerator.", cand, run)
	}
	if contains(cand, string(exec.ConclusionUnmappable)) {
		t.Fatalf("drill_candidate.github_conclusion admits %q — that is OUR word for a conclusion we could not map, and GitHub never writes it. The column would then record our failure as GitHub's verdict, and M is scored against it.",
			exec.ConclusionUnmappable)
	}
	if !contains(cand, string(exec.ConclusionStartupFailure)) {
		t.Fatalf("drill_candidate.github_conclusion does not admit %q. GitHub writes it on real runs (measured 1/10,897 and 7/2,604 concluded runs), and the drill may never drop a candidate GitHub created — so the missing word ABORTS the day's scan, it does not skip a row.",
			exec.ConclusionStartupFailure)
	}
	if len(cand) != 9 {
		t.Fatalf("drill_candidate.github_conclusion has %d members, want the nine of exec.RunConclusions(): %v", len(cand), cand)
	}
	// And the enumerator is what the columns are derived from, so the number
	// above cannot drift away from it without one of the two being wrong.
	if got := len(exec.RunConclusions()); got != 9 {
		t.Fatalf("exec.RunConclusions() has %d members and the two columns hold 9; the derivation and the count disagree", got)
	}
}

// --------------------------------------------------------------------------
// `out_of_scope`'s third meaning, held by GitHub's own word.
// --------------------------------------------------------------------------

// TestOutOfScopeDetailIsAClosedVocabulary drives the first half of 0004's
// mechanism, both ways.
//
// The drill design holds the anti-exclusion rule by what the disposition set
// cannot SAY. `out_of_scope` broke that from the side: three meanings —
// `shadow: never`, not configured, and GitHub's own machinery — landed on one
// enum value separated only by free text, and free text makes everything
// sayable. Until this vocabulary, `out_of_scope` was `deferred` before its
// repair.
func TestOutOfScopeDetailIsAClosedVocabulary(t *testing.T) {
	ctx := context.Background()

	if got := len(OutOfScopeDetails()); got != 3 {
		t.Fatalf("OutOfScopeDetails() has %d members, want 3", got)
	}

	t.Run("each of the three is writable", func(t *testing.T) {
		db := candidateLedger(t)
		for i, d := range OutOfScopeDetails() {
			spec := candSpec{
				id: fmt.Sprintf("ok-%d", i), runID: int64(4000 + i),
				disposition: DispositionOutOfScope, detail: string(d),
			}
			// The third is bound to GitHub's own path and may only be written
			// with it — which is the next test's whole subject.
			if d == DetailGitHubInternalWorkflow {
				spec.path = GitHubInternalPathPrefix + "pages/pages-build-deployment"
			}
			if err := db.RecordExamination(ctx, candDay, []Candidate{spec.candidate()}); err != nil {
				t.Errorf("detail %q is in the Go set and the ledger refused it: %v", d, err)
			}
		}
	})

	t.Run("free text is refused", func(t *testing.T) {
		db := candidateLedger(t)
		for _, detail := range []string{
			"shadow: never", "not configured", "no workflow file",
			"pages-build-deployment", "we could not replay it", "",
		} {
			c := candSpec{
				id: "free", runID: 4100,
				disposition: DispositionOutOfScope, detail: detail,
			}.candidate()
			c.DispositionDetail = detail
			if err := db.RecordExamination(ctx, candDay, []Candidate{c}); err == nil {
				t.Errorf("out_of_scope accepted the free text %q; the three meanings are separated by nothing again", detail)
			}
		}
	})

	// AND THE COLUMN STAYS FREE TEXT EVERYWHERE ELSE. `deferred` has to say
	// WHICH cap was full and no two caps are the same sentence; closing the
	// column outright would delete that in the name of closing a different
	// hole.
	t.Run("the other dispositions keep their sentence", func(t *testing.T) {
		db := candidateLedger(t)
		mustExamine(t, db, candDay,
			candSpec{id: "d1", runID: 4200, disposition: DispositionDeferred,
				detail: "daily cap of 12 reached at 09:14Z"},
			candSpec{id: "d2", runID: 4201, disposition: DispositionNotConcluded,
				detail: "queued for 41 minutes", conclusion: ""},
			candSpec{id: "d3", runID: 4202, disposition: DispositionAlreadySelected,
				detail: "selected on 2026-04-30"},
		)
	})
}

// TestGitHubInternalWorkflowIsHeldByGitHubsOwnPath drives the biconditional,
// both ways, and the three things about it that are not style.
//
// It was measured that GitHub names its own machinery itself: every such run
// carries `path = 'dynamic/<producer>/<name>'` and `event = 'dynamic'`, and the
// two agreed perfectly in both directions over 1,909 runs in three frames. That
// makes the third meaning of `out_of_scope` decidable from GITHUB's side, which
// is the only thing keeping the candidacy/verdict boundary intact — a criterion
// that read OUR side would be "we could not replay it" with a nicer name.
func TestGitHubInternalWorkflowIsHeldByGitHubsOwnPath(t *testing.T) {
	ctx := context.Background()
	const pagesPath = "dynamic/pages/pages-build-deployment"

	t.Run("a dynamic/ run cannot be anything else", func(t *testing.T) {
		db := candidateLedger(t)
		// Every disposition the history path allows, including the two that
		// look most innocent. `not_concluded` is the important one: the
		// biconditional imposes a PRECEDENCE the selector does not get to
		// choose — out_of_scope BEATS not_concluded, because a run outside the
		// universe is outside it whether or not GitHub has judged it yet.
		for i, d := range []Disposition{
			DispositionSelected, DispositionDeferred, DispositionNotConcluded,
			DispositionAlreadySelected,
		} {
			spec := candSpec{
				id: fmt.Sprintf("dyn-%d", i), runID: int64(4300 + i),
				path: pagesPath, disposition: d, detail: "whatever",
			}
			c := spec.candidate()
			if d == DispositionNotConcluded {
				c.GitHubConclusion = ""
			}
			if err := db.RecordExamination(ctx, candDay, []Candidate{c}); err == nil {
				t.Errorf("a %q path was recorded as %q; GitHub's own machinery would enter the corpus and be paid for out of the gate's 5%%", pagesPath, d)
			}
		}
		// And out_of_scope with one of the OTHER two details is refused too:
		// the row would say "the operator excluded it" about a run no operator
		// has ever seen.
		for _, detail := range []OutOfScopeDetail{DetailShadowNever, DetailNotConfigured} {
			c := candSpec{
				id: "dyn-wrong", runID: 4310, path: pagesPath,
				disposition: DispositionOutOfScope, detail: string(detail),
			}.candidate()
			if err := db.RecordExamination(ctx, candDay, []Candidate{c}); err == nil {
				t.Errorf("a %q run was recorded as out_of_scope/%s", pagesPath, detail)
			}
		}
	})

	t.Run("and the claim cannot be made without the path", func(t *testing.T) {
		db := candidateLedger(t)
		for _, path := range []string{
			".github/workflows/ci.yml",
			// Case matters, and this is why the schema uses substr(...) =
			// rather than LIKE: SQLite's LIKE is ASCII-case-INSENSITIVE, so
			// LIKE 'dynamic/%' would let this row into the class.
			"DYNAMIC/pages/pages-build-deployment",
			"dynamic", "adynamic/pages/x", " dynamic/pages/x",
			// The BuildFailed sentinel. A workflow WE wrote that GitHub could
			// not compile: GitHub refuses to name the file, and the run is
			// still OURS. Any rule shaped "not under .github/workflows/ implies
			// not ours" misfiles it.
			"BuildFailed",
		} {
			c := candSpec{
				id: "claim", runID: 4400, path: path,
				disposition: DispositionOutOfScope,
				detail:      string(DetailGitHubInternalWorkflow),
			}.candidate()
			if err := db.RecordExamination(ctx, candDay, []Candidate{c}); err == nil {
				t.Errorf("path %q claimed to be GitHub's own machinery and the ledger accepted it", path)
			}
		}
	})

	// THE BuildFailed RUN IS OURS AND IT IS A CANDIDATE. It carries
	// `startup_failure`, which is the OTHER half of 0004 — so this one row
	// exercises both, and it is the row the negative form of the rule would
	// have thrown away.
	t.Run("BuildFailed with startup_failure is selectable", func(t *testing.T) {
		db := candidateLedger(t)
		mustExamine(t, db, candDay, candSpec{
			id: "buildfailed", runID: 4500, path: "BuildFailed",
			conclusion: exec.ConclusionStartupFailure,
		})
		got, err := db.Candidates(ctx, candDay)
		if err != nil || len(got) != 1 {
			t.Fatalf("Candidates = %d rows, %v", len(got), err)
		}
		if got[0].Disposition != DispositionSelected || got[0].GitHubConclusion != exec.ConclusionStartupFailure {
			t.Fatalf("read back %+v", got[0])
		}
	})

	// THE PRECEDENCE RULE'S ACTUAL PRODUCTION SHAPE: a Pages build GitHub has
	// not concluded yet. It is out_of_scope with NO conclusion at all, which no
	// other disposition allows — `not_concluded` is the value that normally
	// carries a NULL conclusion and the biconditional has just forbidden it
	// here. If this row could not be written, the selector would have nowhere
	// to put an in-flight machinery run and would have to either wait for it or
	// drop it, and dropping it is the thing the drill forbids.
	t.Run("an unconcluded dynamic/ run is still out_of_scope", func(t *testing.T) {
		db := candidateLedger(t)
		c := candSpec{
			id: "inflight", runID: 4700, path: pagesPath,
			disposition: DispositionOutOfScope,
			detail:      string(DetailGitHubInternalWorkflow),
			conclusion:  "",
		}.candidate()
		c.GitHubConclusion = ""
		if err := db.RecordExamination(ctx, candDay, []Candidate{c}); err != nil {
			t.Fatalf("an in-flight run of GitHub's own machinery could not be recorded: %v", err)
		}
	})

	// The prefix is a PREFIX and never a name list: the producer segment is not
	// a closed vocabulary — Copilot review appeared under two different
	// producers, and `agents/...` is a newer spelling of the same thing.
	t.Run("any producer, any name", func(t *testing.T) {
		db := candidateLedger(t)
		for i, path := range []string{
			"dynamic/pages/pages-build-deployment",
			"dynamic/dependabot/dependabot-updates",
			"dynamic/github-code-scanning/codeql",
			"dynamic/copilot-swe-agent/copilot",
			"dynamic/agents/copilot-pull-request-reviewer",
			"dynamic/copilot-pull-request-reviewer/copilot-pull-request-reviewer",
			// A producer nobody has seen yet. A name list would fail here, and
			// failing here means GitHub's machinery entering the corpus.
			"dynamic/some-future-producer/whatever-they-call-it",
		} {
			if err := db.RecordExamination(ctx, candDay, []Candidate{candSpec{
				id: fmt.Sprintf("prod-%d", i), runID: int64(4600 + i), path: path,
				disposition: DispositionOutOfScope,
				detail:      string(DetailGitHubInternalWorkflow),
			}.candidate()}); err != nil {
				t.Errorf("producer path %q was refused: %v", path, err)
			}
		}
	})
}

// TestTheDynamicPrefixIsOneConstantOnBothSides closes the last hand copy in the
// mechanism.
//
// The schema cannot derive `'dynamic/'` from Go — it is a byte string inside a
// substr() call — so the two are read back and compared. The LENGTH matters as
// much as the string: `substr(workflow_path, 1, 8)` with a nine-byte prefix
// silently matches on the first eight bytes and the CHECK stops meaning what it
// says, without erroring anywhere.
func TestTheDynamicPrefixIsOneConstantOnBothSides(t *testing.T) {
	db := candidateLedger(t)
	var ddl string
	if err := db.reader.QueryRowContext(context.Background(),
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'drill_candidate'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("substr(workflow_path, 1, %d) = '%s'", len(GitHubInternalPathPrefix), GitHubInternalPathPrefix)
	if !strings.Contains(stripSQLComments(ddl), want) {
		t.Fatalf("drill_candidate's DDL does not contain %q.\n"+
			"The prefix is a byte string in two places — shadow.GitHubInternalPathPrefix and the CHECK — and nothing derives one from the other. "+
			"If the prefix moved, both have to move, and the substr LENGTH has to move with it.", want)
	}
	// LIKE must not appear in this constraint: it is ASCII-case-INSENSITIVE and
	// would admit `DYNAMIC/`, which is not a string GitHub writes.
	if strings.Contains(strings.ToUpper(stripSQLComments(ddl)), "LIKE '"+strings.ToUpper(GitHubInternalPathPrefix)) {
		t.Error("drill_candidate uses LIKE for the dynamic/ prefix; SQLite's LIKE is ASCII-case-insensitive and this predicate means the exact bytes GitHub writes")
	}
}

// TestTheSchemaHoldsD10WithoutTheGoValidator drives 0004's two CHECKs THROUGH
// RAW SQL, with Candidate.Validate out of the way.
//
// The tests above go through RecordExamination, which validates in Go first —
// so every one of them would still pass with the CHECKs deleted from the
// schema. Two layers were written on purpose (a caller gets a sentence instead
// of a constraint name), and a test that only ever reaches the first one proves
// the second exists nowhere. This is the one that reaches the second.
func TestTheSchemaHoldsD10WithoutTheGoValidator(t *testing.T) {
	const pagesPath = "dynamic/pages/pages-build-deployment"

	insert := func(t *testing.T, db *DB, id, path string, disposition Disposition, detail any) error {
		t.Helper()
		return inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO drill_candidate (
					candidate_id, drill_day, examined_at, selector_rev, source,
					repo, workflow_path, commit_sha, event_name,
					github_run_id, github_run_attempt, github_created_at, github_conclusion,
					disposition, disposition_detail
				) VALUES (?,?,?,?,'github_history', ?,?,?,?, ?,1,?,?, ?,?)`,
				id, candDay, 1_700_000_000_000, "selector-0001",
				"owner/name", path, "0123456789abcdef0123456789abcdef01234567", "push",
				4900, 1_699_000_000_000, "success",
				string(disposition), detail)
			return err
		})
	}

	for _, tc := range []struct {
		name   string
		path   string
		dispo  Disposition
		detail any
		// wantConstraint is empty when the row must be ACCEPTED. Otherwise it
		// is the constraint that must refuse it, by name — because "some
		// constraint said no" is not the assertion. Two of these rows are
		// refused by drill_candidate_exclusion_has_a_reason rather than by the
		// internal-workflow constraints, purely because SQLite evaluates CHECKs in
		// DECLARATION order and that one is declared first. It is the right
		// answer for the wrong reason, and
		// TestTheBiconditionalIsTwoValuedOnItsOwn is what shows the
		// internal-workflow constraints do not depend on it.
		wantConstraint string
	}{
		// The vocabulary.
		{"out_of_scope with free text", ".github/workflows/ci.yml", DispositionOutOfScope, "not configured", "drill_candidate_out_of_scope_detail_enum"},
		{"out_of_scope shadow_never", ".github/workflows/ci.yml", DispositionOutOfScope, string(DetailShadowNever), ""},
		{"out_of_scope not_configured", ".github/workflows/ci.yml", DispositionOutOfScope, string(DetailNotConfigured), ""},

		// THE NULL HOLE, and the reason this case is in the list. A CHECK that
		// evaluates to NULL PASSES in SQLite, so as the biconditional was
		// PROPOSED — `disposition = 'out_of_scope' AND disposition_detail =
		// 'github_internal_workflow'` — this row's right-hand side was
		// `TRUE AND NULL` = NULL and the whole constraint was void. The only
		// thing refusing it was drill_candidate_exclusion_has_a_reason, a
		// constraint in another paragraph written for another reason. 0004
		// spells both sides so they cannot be NULL; this row must be refused by
		// its OWN constraint, not by a neighbour's.
		{"out_of_scope with a NULL detail", ".github/workflows/ci.yml", DispositionOutOfScope, nil, "drill_candidate_exclusion_has_a_reason"},
		{"dynamic/ out_of_scope with a NULL detail", pagesPath, DispositionOutOfScope, nil, "drill_candidate_exclusion_has_a_reason"},

		// The biconditional, both directions.
		{"dynamic/ correctly filed", pagesPath, DispositionOutOfScope, string(DetailGitHubInternalWorkflow), ""},
		{"dynamic/ selected", pagesPath, DispositionSelected, nil, "drill_candidate_github_internal_is_dynamic_path"},
		{"dynamic/ deferred", pagesPath, DispositionDeferred, "daily cap", "drill_candidate_github_internal_is_dynamic_path"},
		{"dynamic/ already_selected", pagesPath, DispositionAlreadySelected, "seen on 2026-04-30", "drill_candidate_github_internal_is_dynamic_path"},
		{"dynamic/ out_of_scope but shadow_never", pagesPath, DispositionOutOfScope, string(DetailShadowNever), "drill_candidate_github_internal_is_dynamic_path"},
		{"ordinary path claiming the class", ".github/workflows/ci.yml", DispositionOutOfScope, string(DetailGitHubInternalWorkflow), "drill_candidate_github_internal_is_dynamic_path"},
		{"BuildFailed claiming the class", "BuildFailed", DispositionOutOfScope, string(DetailGitHubInternalWorkflow), "drill_candidate_github_internal_is_dynamic_path"},
		{"BuildFailed is ours and selectable", "BuildFailed", DispositionSelected, nil, ""},
		// LIKE would admit this one. substr(...) = does not.
		{"DYNAMIC/ is not the class", "DYNAMIC/pages/pages-build-deployment", DispositionSelected, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := candidateLedger(t)
			err := insert(t, db, "raw", tc.path, tc.dispo, tc.detail)
			switch {
			case tc.wantConstraint == "" && err != nil:
				t.Fatalf("the schema refused a legal row: %v", err)
			case tc.wantConstraint == "":
			case err == nil:
				t.Fatalf("the schema ACCEPTED the row; the Go validator is the only thing holding this rule")
			case !strings.Contains(err.Error(), tc.wantConstraint):
				t.Fatalf("the row was refused by the wrong constraint:\n got  %v\n want %s", err, tc.wantConstraint)
			}
		})
	}
}

// TestTheBiconditionalIsTwoValuedOnItsOwn executes the divergence between the
// PROPOSED internal-workflow predicate and the one 0004 ships, on two throwaway tables that
// carry nothing else.
//
// The proposed spelling
//
//	(substr(workflow_path,1,8) = 'dynamic/')
//	  = (disposition = 'out_of_scope' AND disposition_detail = 'github_internal_workflow')
//
// evaluates to NULL when disposition_detail is NULL — `TRUE AND NULL` is NULL —
// and SQLITE PASSES A CHECK THAT EVALUATES TO NULL. On the real table the row
// is still refused, but by drill_candidate_exclusion_has_a_reason, a constraint
// written for a different rule and declared first. A discriminator held by a
// neighbour is one relaxation away from passing everything without erroring,
// and this project has now paid for that same NULL rule four times.
//
// These tables have no neighbour, so the difference is visible: the proposed
// one ACCEPTS the row and the shipped one REFUSES it.
func TestTheBiconditionalIsTwoValuedOnItsOwn(t *testing.T) {
	db := candidateLedger(t)

	err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		if _, err := tx.ExecContext(ctx, `
			CREATE TEMP TABLE d10_proposed (
			    workflow_path      TEXT NOT NULL,
			    disposition        TEXT NOT NULL,
			    disposition_detail TEXT,
			    CHECK ((substr(workflow_path, 1, 8) = 'dynamic/')
			           = (disposition = 'out_of_scope'
			              AND disposition_detail = 'github_internal_workflow')));
			CREATE TEMP TABLE d10_shipped (
			    workflow_path      TEXT NOT NULL,
			    disposition        TEXT NOT NULL,
			    disposition_detail TEXT,
			    CHECK ((substr(workflow_path, 1, 8) = 'dynamic/')
			           = (disposition = 'out_of_scope'
			              AND disposition_detail IS NOT NULL
			              AND disposition_detail = 'github_internal_workflow')));`); err != nil {
			return err
		}

		const row = `INSERT INTO %s VALUES ('dynamic/pages/pages-build-deployment', 'out_of_scope', NULL)`
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(row, "temp.d10_proposed")); err != nil {
			t.Errorf("the PROPOSED biconditional refused the NULL-detail row: %v.\n"+
				"If SQLite has stopped passing CHECKs that evaluate to NULL, the divergence 0004 records is no longer needed and this test should say so.", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(row, "temp.d10_shipped")); err == nil {
			t.Error("the SHIPPED biconditional accepted a dynamic/ row with a NULL detail; the NULL hole is back and the rule is being held by drill_candidate_exclusion_has_a_reason again")
		}

		// And the shipped one still accepts the row that is actually correct,
		// so the repair narrowed nothing it should not have.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO temp.d10_shipped VALUES ('dynamic/pages/pages-build-deployment', 'out_of_scope', 'github_internal_workflow')`); err != nil {
			t.Errorf("the shipped biconditional refused a correctly filed row: %v", err)
		}

		_, err := tx.ExecContext(ctx, `DROP TABLE temp.d10_proposed; DROP TABLE temp.d10_shipped;`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheSchemaHoldsStartupFailure is the same argument for part 1: the two
// CHECKs, driven with Validate out of the way, in both directions.
func TestTheSchemaHoldsStartupFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("drill_candidate.github_conclusion", func(t *testing.T) {
		db := candidateLedger(t)
		for _, tc := range []struct {
			conclusion  string
			wantRefusal bool
		}{
			{"startup_failure", false},
			{"success", false},
			{"stale", false},
			// Ours, never GitHub's. It would record our failure to map a
			// conclusion as GitHub's verdict, and M is scored against it.
			{"unmappable", true},
			// GitHub's `status`, not its `conclusion`.
			{"in_progress", true},
		} {
			err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO drill_candidate (
						candidate_id, drill_day, examined_at, selector_rev, source,
						repo, workflow_path, commit_sha, event_name,
						github_run_id, github_run_attempt, github_created_at, github_conclusion,
						disposition, disposition_detail
					) VALUES (?,?,?,?,'github_history', ?,?,?,?, ?,1,?,?, 'selected', NULL)`,
					"sf-"+tc.conclusion, candDay, 1_700_000_000_000, "selector-0001",
					"owner/name", ".github/workflows/ci.yml",
					"0123456789abcdef0123456789abcdef01234567", "push",
					5000+int64(len(tc.conclusion)), 1_699_000_000_000, tc.conclusion)
				return err
			})
			switch {
			case tc.wantRefusal && err == nil:
				t.Errorf("github_conclusion accepted %q", tc.conclusion)
			case !tc.wantRefusal && err != nil:
				t.Errorf("github_conclusion refused %q: %v — that aborts the day's scan, it does not skip a row", tc.conclusion, err)
			}
		}
	})

	// The run column is the OTHER side of the comparison that produces M. It
	// goes through Verdict.Validate, which has admitted the value since
	// exec.Conclusion.IsGitHub() did — so before 0004 Go accepted the row and
	// the schema aborted the INSERT. That window is what this closes.
	t.Run("exec_invocation.run_conclusion", func(t *testing.T) {
		db := candidateLedger(t)
		mustOpen(t, db, invSpec{id: "sf-run", day: candDay})
		err := db.CloseInvocation(ctx, Verdict{
			InvocationID:  "sf-run",
			FinishedAt:    time.UnixMilli(1_700_000_001_000),
			Outcome:       exec.OutcomeCompleted,
			RunConclusion: exec.ConclusionStartupFailure,
			Comparison:    exec.ComparisonMismatched,
			Jobs: []Job{{
				JobID:      "build",
				Leg:        exec.CanonicalLegKeyFromValues(map[string]any{}),
				Source:     SourceExecutor,
				Conclusion: exec.ConclusionFailure,
			}},
		})
		if err != nil {
			t.Fatalf("closing a run with %q was refused: %v", exec.ConclusionStartupFailure, err)
		}
	})
}
