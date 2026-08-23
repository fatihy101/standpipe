package shadow

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

var namedConstraint = regexp.MustCompile(`CONSTRAINT\s+([a-z_0-9]+)`)

// namedConstraintsOn returns every CONSTRAINT name the given migration declares
// whose name is prefixed with the table's own — that prefix is the convention
// 0002 introduced and it is what makes this readable off the source.
func namedConstraintsOn(t *testing.T, migration, table string) []string {
	t.Helper()
	var src string
	for _, m := range migrations {
		if m.Name == migration {
			src = m.Source
		}
	}
	if src == "" {
		t.Fatalf("no embedded migration named %s", migration)
	}
	var out []string
	for _, m := range namedConstraint.FindAllStringSubmatch(stripSQLComments(src), -1) {
		if strings.HasPrefix(m[1], table+"_") {
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no named constraint on %s; the scan is broken", migration, table)
	}
	return out
}

// 0004 REBUILDS TWO TABLES, AND EVERY TEST ABOVE MIGRATES AN EMPTY FILE.
//
// An empty rebuild exercises none of what makes a rebuild dangerous. The
// deferred foreign-key counter only moves when there are CHILD ROWS; the
// re-insert only proves anything when there is something to re-insert; and the
// "failing copy is the feature" rule only means something when a row exists
// that the new schema cannot hold. This file is where a POPULATED ledger goes
// from schema 3 to schema 4.
//
// The specific hazard, measured for 0002:
// `PRAGMA defer_foreign_keys` defers a COUNTER, it does not re-check at COMMIT.
// DROP TABLE increments it once per child row and ALTER TABLE ... RENAME TO
// never decrements it, so the SQLite manual's copy-then-drop order COMMITs with
// SQLITE_CONSTRAINT_FOREIGNKEY (787) on a database that is entirely consistent.
// exec_invocation now has SEVEN children and drill_candidate has one. Only the
// re-insert clears the counter, and only a populated file can show it.

// ledgerAtSchema returns an open ledger migrated to exactly version v.
func ledgerAtSchema(t *testing.T, v int) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := OpenUnchecked(ctx, Config{Path: filepath.Join(t.TempDir(), "shadow.db")})
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	for _, m := range migrations {
		if m.Version > v {
			break
		}
		if err := db.applyMigration(ctx, m); err != nil {
			t.Fatalf("applying migration %d: %v", m.Version, err)
		}
	}
	if got, err := db.SchemaVersion(ctx); err != nil || got != v {
		t.Fatalf("schema version = %d, %v; want %d", got, err, v)
	}
	return db
}

// populateAtSchema3 writes the shape the drill actually produces: days, a zone,
// paired drill runs with their children, candidates in several dispositions and
// attempts pointing at invocations. Every foreign key 0004's two DROPs will
// walk has rows behind it.
func populateAtSchema3(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	mustZone(t, db, "2026-01-01", "Europe/Istanbul")
	mustDay(t, db, candDay, DayRan, "")
	mustDay(t, db, "2026-05-02", DayRan, "")

	// A selected candidate, planned in both modes, with an invocation and its
	// six child tables behind each. This is what makes the counter move.
	mustPairedDrill(t, db, candDay, "p1", 8001)
	mustPairedDrill(t, db, "2026-05-02", "p2", 8002)

	// Rejected candidates, and an OPEN invocation with no verdict — the row
	// whose NULL outcome the carried CHECKs have to keep accepting.
	mustExamine(t, db, candDay,
		candSpec{id: "r1", runID: 8003, disposition: DispositionDeferred, detail: "daily cap of 12"},
		candSpec{id: "r2", runID: 8004, disposition: DispositionNotConcluded, detail: "queued", conclusion: ""},
		candSpec{id: "r3", runID: 8005, disposition: DispositionOutOfScope, detail: string(DetailNotConfigured)},
	)
	mustOpen(t, db, invSpec{id: "open-1", day: candDay})
	_ = ctx
}

func tableRows(t *testing.T, db *DB, table string) int {
	t.Helper()
	var n int
	if err := db.reader.QueryRowContext(context.Background(),
		"SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func rowIdentity(t *testing.T, db *DB, query string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	rows, err := db.reader.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var rowid int64
		if err := rows.Scan(&id, &rowid); err != nil {
			t.Fatal(err)
		}
		out[id] = rowid
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMigration0004OnAPopulatedLedger is the drive.
func TestMigration0004OnAPopulatedLedger(t *testing.T) {
	ctx := context.Background()
	db := ledgerAtSchema(t, 3)
	populateAtSchema3(t, db)

	before := map[string]int{}
	for _, table := range []string{
		"drill_day", "drill_day_zone", "exec_invocation", "exec_image",
		"exec_local_repository", "exec_job_result", "exec_step_observation",
		"exec_residue", "exec_note", "drill_candidate", "drill_attempt",
	} {
		before[table] = tableRows(t, db, table)
		if before[table] == 0 {
			t.Fatalf("%s is empty before the migration; this test would prove nothing about it", table)
		}
	}
	invRowids := rowIdentity(t, db, "SELECT invocation_id, rowid FROM exec_invocation")
	candRowids := rowIdentity(t, db, "SELECT candidate_id, rowid FROM drill_candidate")

	steps, err := db.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate to 4 on a populated ledger: %v\n"+
			"SQLITE_CONSTRAINT_FOREIGNKEY (787) here means the re-insert is not clearing the deferred counter the DROP raised.", err)
	}
	if len(steps) != 1 || steps[0].Version != 4 {
		t.Fatalf("Migrate applied %+v, want only version 4", steps)
	}

	// 1. NOTHING WAS LOST, in either table or in any child.
	for table, want := range before {
		if got := tableRows(t, db, table); got != want {
			t.Errorf("%s has %d rows after the migration, had %d", table, got, want)
		}
	}

	// 2. ROWIDS ARE PRESERVED. Nothing in this package orders by rowid today,
	//    which is exactly why losing them would be discovered late, in an
	//    export diff, rather than here.
	for id, want := range invRowids {
		if got := rowIdentity(t, db, "SELECT invocation_id, rowid FROM exec_invocation")[id]; got != want {
			t.Errorf("exec_invocation %s moved from rowid %d to %d", id, want, got)
		}
	}
	for id, want := range candRowids {
		if got := rowIdentity(t, db, "SELECT candidate_id, rowid FROM drill_candidate")[id]; got != want {
			t.Errorf("drill_candidate %s moved from rowid %d to %d", id, want, got)
		}
	}

	// 3. THE DATABASE IS SOUND, by SQLite's own reading and not by ours.
	var integrity string
	if err := db.reader.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity_check = %q, %v", integrity, err)
	}
	fkRows, err := db.reader.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	for fkRows.Next() {
		var a, b, c, d any
		_ = fkRows.Scan(&a, &b, &c, &d)
		t.Errorf("foreign_key_check: %v %v %v %v", a, b, c, d)
	}
	fkRows.Close()

	// 4. THE FOREIGN KEYS ARE STILL ENFORCED. A rebuild that silently dropped
	//    them would pass every assertion above.
	if err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO drill_attempt (candidate_id, supply_mode, planned_at) VALUES ('no-such-candidate', 'online', 1)`)
		return err
	}); err == nil {
		t.Error("drill_attempt accepted a candidate_id that does not exist; the FK did not survive the rebuild")
	}

	// 5. THE TRIGGERS ARE BACK, all five that the two DROPs took with them.
	if err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx, `UPDATE drill_candidate SET repo = 'other' WHERE candidate_id = 'p1'`)
		return err
	}); err == nil {
		t.Error("drill_candidate is no longer append-only")
	}
	if err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx, `UPDATE exec_invocation SET repo = 'other' WHERE invocation_id = 'open-1'`)
		return err
	}); err == nil {
		t.Error("exec_invocation_write_once is gone")
	}
	if err := db.PlanAttempts(ctx, "r3", time.UnixMilli(1_700_000_000_000)); err == nil {
		t.Error("an attempt was planned for an out_of_scope candidate; drill_attempt_only_for_selected did not come back")
	}

	// 6. AND THE PARTIAL UNIQUE INDEX IS BACK — selection is once-ever, which
	//    is what makes the promotion gate's "20 DISTINCT runs" equal a row count.
	if err := db.RecordExamination(ctx, "2026-05-02", []Candidate{
		candSpec{id: "again", day: "2026-05-02", runID: 8001}.candidate(),
	}); err == nil {
		t.Error("the same GitHub run was selected twice; idx_drill_candidate_selected_once did not come back")
	}

	// 7. NO CONSTRAINT WAS LOST IN THE REBUILD. A rebuild is a retyped copy of
	//    a table, and a retyped copy is where a CHECK goes missing without
	//    anything failing — the rows that would have violated it are simply not
	//    in the fixture. The names are read out of the migrations that DEFINED
	//    them and out of the live schema, so this is a derivation rather than a
	//    list somebody keeps up to date.
	for _, tc := range []struct{ definedIn, table string }{
		{"0002_two_phase_and_comparison", "exec_invocation"},
		{"0003_drill_candidate_axis", "drill_candidate"},
	} {
		var live string
		if err := db.reader.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, tc.table).Scan(&live); err != nil {
			t.Fatal(err)
		}
		live = stripSQLComments(live)
		for _, name := range namedConstraintsOn(t, tc.definedIn, tc.table) {
			if !strings.Contains(live, "CONSTRAINT "+name+" ") && !strings.Contains(live, "CONSTRAINT "+name+"\n") {
				t.Errorf("%s lost the constraint %s in 0004's rebuild; it was defined in %s and nothing in this change was supposed to remove it",
					tc.table, name, tc.definedIn)
			}
		}
	}

	// 8. The widened columns actually admit the new value, on the real tables.
	if err := db.RecordExamination(ctx, "2026-05-02", []Candidate{
		candSpec{id: "sf", day: "2026-05-02", runID: 8100, conclusion: exec.ConclusionStartupFailure}.candidate(),
	}); err != nil {
		t.Errorf("a candidate carrying startup_failure was refused after the migration: %v", err)
	}
}

// TestMigration0004AbortsOnARowItCannotHold is the other half, and the abort is
// the FEATURE.
//
// The two classes that can block it are exactly the defect 0004 names: an
// `out_of_scope` row whose detail is free text, and a `dynamic/` row the
// selector did not file as GitHub's machinery. Neither may be rewritten by the
// migration — writing 'github_internal_workflow' over an operator's own
// sentence is repair-by-fabrication — so the file aborts, and what matters is
// that it aborts WHOLLY: schema 3, every row intact, user_version unchanged, and
// the same file migrates once the row is dealt with.
func TestMigration0004AbortsOnARowItCannotHold(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		path   string
		dispo  Disposition
		detail string
	}{
		{"free text under out_of_scope", ".github/workflows/ci.yml", DispositionOutOfScope, "no workflow file"},
		{"a dynamic/ run filed as a candidate", "dynamic/pages/pages-build-deployment", DispositionSelected, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := ledgerAtSchema(t, 3)
			populateAtSchema3(t, db)

			// Raw SQL: Candidate.Validate holds these rules too, so the only
			// way to produce a row that is legal under 3 and illegal under 4 is
			// to go around it — which is also how such a row would have got
			// there in the first place, from a build that predates 0004.
			var detail any
			if tc.detail != "" {
				detail = tc.detail
			}
			if err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
				_, err := tx.ExecContext(ctx, `
					INSERT INTO drill_candidate (
						candidate_id, drill_day, examined_at, selector_rev, source,
						repo, workflow_path, commit_sha, event_name,
						github_run_id, github_run_attempt, github_created_at, github_conclusion,
						disposition, disposition_detail
					) VALUES ('legacy', ?, 1700000000000, 'selector-0000', 'github_history',
					          'owner/name', ?, '0123456789abcdef0123456789abcdef01234567', 'push',
					          9999, 1, 1699000000000, 'success', ?, ?)`,
					candDay, tc.path, string(tc.dispo), detail)
				return err
			}); err != nil {
				t.Fatalf("schema 3 refused a row it was supposed to accept: %v", err)
			}
			candidatesBefore := tableRows(t, db, "drill_candidate")
			invocationsBefore := tableRows(t, db, "exec_invocation")

			if _, err := db.Migrate(ctx); err == nil {
				t.Fatal("the migration accepted a row the new schema cannot hold")
			}

			// ROLLED BACK WHOLLY: still schema 3, still every row, and the two
			// rebuilt tables are still there with their data.
			if got, err := db.SchemaVersion(ctx); err != nil || got != 3 {
				t.Fatalf("schema version = %d, %v after an aborted migration; want 3", got, err)
			}
			if got := tableRows(t, db, "drill_candidate"); got != candidatesBefore {
				t.Errorf("drill_candidate has %d rows after the abort, had %d", got, candidatesBefore)
			}
			if got := tableRows(t, db, "exec_invocation"); got != invocationsBefore {
				t.Errorf("exec_invocation has %d rows after the abort, had %d", got, invocationsBefore)
			}

			// AND THE RE-RUN AFTER THE REPAIR SUCCEEDS. The TEMP carry tables
			// must not survive the rollback on the pooled writer connection; if
			// they did, only the SECOND attempt would fail, which is the worst
			// possible place to find out.
			if err := inWriteTx(t, db, func(ctx context.Context, tx *sql.Conn) error {
				_, err := tx.ExecContext(ctx, `DELETE FROM drill_candidate WHERE candidate_id = 'legacy'`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Migrate(ctx); err != nil {
				t.Fatalf("the migration failed on its second attempt after the blocking row was removed: %v", err)
			}
			if got, err := db.SchemaVersion(ctx); err != nil || got != LatestSchemaVersion() {
				t.Fatalf("schema version = %d, %v; want %d", got, err, LatestSchemaVersion())
			}
		})
	}
}
