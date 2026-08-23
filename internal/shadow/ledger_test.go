package shadow

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Helpers shared by this package's tests.
//
// They live in the package under test rather than in internal/testutil for the
// same reason store's do: the proofs they support need the UNEXPORTED purges
// and the unexported audit helpers, which is the whole subject of the single-call-site rule:
// an ordering rule no schema can hold gets exactly one exported entry point.

// newLedger returns a migrated, empty shadow ledger on a temp file.
func newLedger(t *testing.T) *DB {
	t.Helper()
	ctx := context.Background()
	db, err := OpenUnchecked(ctx, Config{Path: filepath.Join(t.TempDir(), "shadow.db")})
	if err != nil {
		t.Fatalf("OpenUnchecked: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func mustDay(t *testing.T, db *DB, day string, status DayStatus, reason DropReason) {
	t.Helper()
	if err := db.RecordDay(context.Background(), Day{Day: day, Status: status, Reason: reason}); err != nil {
		t.Fatalf("RecordDay(%s, %s): %v", day, status, err)
	}
}

// invSpec is the minimum a test needs to say about a run.
type invSpec struct {
	id        string
	day       string
	startedAt time.Time
	purpose   exec.Purpose
	mode      exec.SupplyMode

	// comparison is empty for "matched". It is spelled out only by the tests
	// that need a MISMATCH, which is the whole subject of the "gate never reads through the
	// candidate axis" measurement.
	comparison exec.Comparison
}

func (s invSpec) invocation() Invocation {
	p := s.purpose
	if p == "" {
		p = exec.PurposeDrill
	}
	m := s.mode
	if m == "" {
		m = exec.SupplyOnline
	}
	started := s.startedAt
	if started.IsZero() {
		started = time.UnixMilli(1_700_000_000_000)
	}
	shallow := false
	return Invocation{
		InvocationID:       s.id,
		BackendName:        "act-local",
		Identity:           exec.Identity{Kind: "act", Version: "0.2.89", VersionPinOK: true},
		StartedAt:          started,
		Repo:               "owner/name",
		WorkflowPath:       ".github/workflows/test.yml",
		CommitSHA:          "0123456789abcdef0123456789abcdef01234567",
		EventName:          "push",
		EventPayloadSHA256: "payload-sha",
		Purpose:            p,
		DrillDay:           s.day,
		SupplyMode:         m,
		Preflight: &Preflight{
			ArgvSHA256:          "argv-sha",
			WorkflowNormSHA256:  "norm-sha",
			WorkflowNormApplied: true,
			Workdir:             exec.WorkdirProvenance{Clean: true, HeadSHA: "head", CommitCount: 3, Shallow: &shallow},
		},
		Images: []Image{{
			RunsOnLabel: "ubuntu-24.04", LocalTag: "standpipe/ubuntu:24.04",
			SourceDigest: "sha256:" + strings.Repeat("a", 64),
			Architecture: "amd64", ConfigNonEmpty: true, PathPresent: true,
		}},
		LocalRepositories: []LocalRepository{{
			URL: "https://github.com/actions/checkout", LocalPath: "/srv/mirrors/checkout",
			ResolvedSHA: strings.Repeat("b", 40),
		}},
		Notes: []string{"argv note"},
	}
}

func mustOpen(t *testing.T, db *DB, s invSpec) {
	t.Helper()
	if err := db.OpenInvocation(context.Background(), s.invocation()); err != nil {
		t.Fatalf("OpenInvocation(%s): %v", s.id, err)
	}
}

// mustDrillRun opens and closes a completed, matched drill run, which is the
// shape that keeps audit 2 quiet for a `ran` day.
func mustDrillRun(t *testing.T, db *DB, s invSpec) {
	t.Helper()
	mustOpen(t, db, s)
	comparison := s.comparison
	if comparison == "" {
		comparison = exec.ComparisonMatched
	}
	leg := exec.CanonicalLegKeyFromValues(map[string]any{})
	err := db.CloseInvocation(context.Background(), Verdict{
		InvocationID:  s.id,
		FinishedAt:    time.UnixMilli(1_700_000_100_000),
		Outcome:       exec.OutcomeCompleted,
		RunConclusion: exec.ConclusionSuccess,
		Comparison:    comparison,
		Jobs: []Job{{
			JobID:      "build",
			Leg:        leg,
			Source:     SourceExecutor,
			Conclusion: exec.ConclusionSuccess,
			Steps: []Step{
				{Ordinal: 1, StepID: []string{"0"}, Name: "checkout", Ran: true, Conclusion: exec.ConclusionSuccess},
			},
		}},
		Residue: []Residue{{Kind: "container", Ref: "c1", ObservedAt: time.UnixMilli(1_700_000_100_000)}},
		Notes:   []string{"an .actrc was present"},
	})
	if err != nil {
		t.Fatalf("CloseInvocation(%s): %v", s.id, err)
	}
}

// rowCounts is the whole ledger's shape in one value, so "nothing was deleted"
// is one comparison rather than six.
type rowCounts struct {
	Days        int
	Zones       int
	Candidates  int
	Attempts    int
	Invocations int
	Jobs        int
	Steps       int
	Images      int
	Residue     int
	LocalRepos  int
	Notes       int
}

func countRows(t *testing.T, db *DB) rowCounts {
	t.Helper()
	var c rowCounts
	for _, pair := range []struct {
		table string
		into  *int
	}{
		{"drill_day", &c.Days},
		{"drill_day_zone", &c.Zones},
		{"drill_candidate", &c.Candidates},
		{"drill_attempt", &c.Attempts},
		{"exec_invocation", &c.Invocations},
		{"exec_job_result", &c.Jobs},
		{"exec_step_observation", &c.Steps},
		{"exec_image", &c.Images},
		{"exec_residue", &c.Residue},
		{"exec_local_repository", &c.LocalRepos},
		{"exec_note", &c.Notes},
	} {
		if err := db.reader.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM `+pair.table).Scan(pair.into); err != nil {
			t.Fatalf("counting %s: %v", pair.table, err)
		}
	}
	return c
}

func dayExists(t *testing.T, db *DB, day string) bool {
	t.Helper()
	var n int
	if err := db.reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM drill_day WHERE day = ?`, day).Scan(&n); err != nil {
		t.Fatalf("dayExists(%s): %v", day, err)
	}
	return n == 1
}

func invocationExists(t *testing.T, db *DB, id string) bool {
	t.Helper()
	var n int
	if err := db.reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM exec_invocation WHERE invocation_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("invocationExists(%s): %v", id, err)
	}
	return n == 1
}

// inWriteTx runs fn on the writer connection inside BEGIN IMMEDIATE, so a test
// can reach the unexported purges the way Retain does.
func inWriteTx(t *testing.T, db *DB, fn func(ctx context.Context, tx *sql.Conn) error) error {
	t.Helper()
	return db.writeTx(context.Background(), fn)
}

// --------------------------------------------------------------------------
// The candidate axis, as fixtures.
// --------------------------------------------------------------------------

func mustZone(t *testing.T, db *DB, sinceDay, tz string) {
	t.Helper()
	if err := db.RecordDayZone(context.Background(), DayZone{SinceDay: sinceDay, Timezone: tz}); err != nil {
		t.Fatalf("RecordDayZone(%s, %s): %v", sinceDay, tz, err)
	}
}

// candSpec is the minimum a test needs to say about an examined run. Everything
// else is filled with a shape the schema accepts, so a test that is about
// retention does not have to be about the DDL.
type candSpec struct {
	id          string
	day         string
	runID       int64
	disposition Disposition
	detail      string
	source      CandidateSource
	conclusion  exec.Conclusion
	createdAt   time.Time

	// path overrides the workflow path. It is spelled out only by the tests
	// about GitHub's own internal workflows, where the path is no longer scenery: since 0004 a path under
	// GitHubInternalPathPrefix DECIDES the disposition.
	path string
}

func (s candSpec) candidate() Candidate {
	src := s.source
	if src == "" {
		src = SourceGitHubHistory
	}
	disp := s.disposition
	if disp == "" {
		disp = DispositionSelected
	}
	path := s.path
	if path == "" {
		path = ".github/workflows/test.yml"
	}
	detail := s.detail
	if disp != DispositionSelected && detail == "" {
		// `out_of_scope` has a CLOSED vocabulary since 0004; "fixture" is not
		// in it. The default is the meaning that says the least about the run
		// itself, so a test that is not about those does not accidentally claim
		// GitHub's machinery.
		detail = "fixture"
		if disp == DispositionOutOfScope {
			detail = string(DetailNotConfigured)
		}
		if IsGitHubInternalWorkflowPath(path) {
			detail = string(DetailGitHubInternalWorkflow)
		}
	}
	conclusion := s.conclusion
	if conclusion == "" && disp != DispositionNotConcluded {
		conclusion = exec.ConclusionSuccess
	}
	created := s.createdAt
	if created.IsZero() {
		created = time.UnixMilli(1_699_000_000_000)
	}
	c := Candidate{
		CandidateID:       s.id,
		DrillDay:          s.day,
		ExaminedAt:        time.UnixMilli(1_700_000_000_000),
		SelectorRev:       "selector-0001",
		Source:            src,
		Repo:              "owner/name",
		WorkflowPath:      path,
		CommitSHA:         "0123456789abcdef0123456789abcdef01234567",
		EventName:         "push",
		GitHubConclusion:  conclusion,
		Disposition:       disp,
		DispositionDetail: detail,
	}
	if src == SourceGitHubHistory {
		runID := s.runID
		if runID == 0 {
			runID = 1
		}
		c.GitHubRunID = runID
		c.GitHubRunAttempt = 1
		c.GitHubCreatedAt = created
	}
	return c
}

func mustExamine(t *testing.T, db *DB, day string, specs ...candSpec) {
	t.Helper()
	cands := make([]Candidate, 0, len(specs))
	for _, s := range specs {
		if s.day == "" {
			s.day = day
		}
		cands = append(cands, s.candidate())
	}
	if err := db.RecordExamination(context.Background(), day, cands); err != nil {
		t.Fatalf("RecordExamination(%s): %v", day, err)
	}
}

// mustPairedDrill drives the drill's whole order for ONE candidate: examine, plan
// BOTH modes, open and close an invocation for each, and link them.
//
// It exists so that the retention tests are exercised against the shape the
// drill actually produces. A fixture that wrote a candidate with no attempts,
// or attempts with no invocations, would leave the foreign keys retention has
// to respect untested in exactly the direction they were found broken.
func mustPairedDrill(t *testing.T, db *DB, day, candidateID string, runID int64) {
	t.Helper()
	mustExamine(t, db, day, candSpec{id: candidateID, day: day, runID: runID})
	if err := db.PlanAttempts(context.Background(), candidateID, time.UnixMilli(1_700_000_000_000)); err != nil {
		t.Fatalf("PlanAttempts(%s): %v", candidateID, err)
	}
	for _, mode := range exec.SupplyModes() {
		invID := candidateID + "-" + string(mode)
		mustDrillRun(t, db, invSpec{id: invID, day: day, mode: mode})
		if err := db.LinkInvocation(context.Background(), candidateID, mode, invID); err != nil {
			t.Fatalf("LinkInvocation(%s, %s): %v", candidateID, mode, err)
		}
	}
}

func candidateExists(t *testing.T, db *DB, id string) bool {
	t.Helper()
	var n int
	if err := db.reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM drill_candidate WHERE candidate_id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("candidateExists(%s): %v", id, err)
	}
	return n == 1
}

func attemptCount(t *testing.T, db *DB, candidateID string) int {
	t.Helper()
	var n int
	if err := db.reader.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM drill_attempt WHERE candidate_id = ?`, candidateID).Scan(&n); err != nil {
		t.Fatalf("attemptCount(%s): %v", candidateID, err)
	}
	return n
}
