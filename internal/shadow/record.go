package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
	"modernc.org/sqlite"
)

// SQLite EXTENDED constraint result codes. The primary code (19) cannot tell a
// foreign-key failure from a trigger's RAISE(ABORT), and this package has to:
// an unrecorded drill day and a forbidden relabelling are different sentences
// to whoever is reading the log at 3am.
const (
	constraintCheck      = 275  // SQLITE_CONSTRAINT_CHECK
	constraintForeignKey = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	constraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
	constraintTrigger    = 1811 // SQLITE_CONSTRAINT_TRIGGER
	constraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
)

// extendedCode returns the driver's extended result code, or -1 when err did
// not come from the driver.
func extendedCode(err error) int {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return -1
	}
	return serr.Code()
}

// RecordDay writes or updates one drill_day row.
//
// The day axis is not derived from the run axis. A day exists because the drill
// scheduler says it does; the absence of a row is a recording defect, and if
// this call were made implicitly from the first invocation, a day on which
// nothing was attempted would simply have no row and would look like a day that
// had not arrived yet.
//
// The upsert is deliberate and asymmetric. `dropped` -> `ran` is legal: a day
// recorded dropped in the morning can turn out to have run. `ran` -> `dropped`
// is refused by a trigger and comes back as ErrDayDowngrade, because it invents
// an absence reason for a day that was not absent — the day axis's copy of
// "purpose is written before the run and never rewritten".
//
// Re-dropping an already-dropped day with a different reason OVERWRITES the
// reason. A day carries one reason; the previous one is not kept.
func (db *DB) RecordDay(ctx context.Context, d Day) error {
	if err := d.Validate(); err != nil {
		return err
	}
	var reason any
	if d.Status == DayDropped {
		reason = string(d.Reason)
	}
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO drill_day (day, status, drop_reason) VALUES (?, ?, ?)
			 ON CONFLICT(day) DO UPDATE SET status = excluded.status, drop_reason = excluded.drop_reason`,
			d.Day, string(d.Status), reason)
		return err
	})
	if err != nil && extendedCode(err) == constraintTrigger {
		return fmt.Errorf("%w: %s", ErrDayDowngrade, d.Day)
	}
	return err
}

// Days reads the whole window, ordered by day.
func (db *DB) Days(ctx context.Context) ([]Day, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT day, status, drop_reason FROM drill_day ORDER BY day`)
	if err != nil {
		return nil, fmt.Errorf("shadow: reading drill_day: %w", err)
	}
	defer rows.Close()
	var out []Day
	for rows.Next() {
		var d Day
		var reason sql.NullString
		if err := rows.Scan(&d.Day, &d.Status, &reason); err != nil {
			return nil, err
		}
		d.Reason = DropReason(reason.String)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// The run axis is written in TWO PHASES.
//
// OpenInvocation writes the row BEFORE the process is invoked, with outcome
// NULL. CloseInvocation writes the verdict. The reason is one sentence: A
// CRASHED PROCESS CANNOT EMIT ITS OWN COUNTER. A record written only after the
// run leaves nothing behind when the harness dies mid-run — and such a run was
// ATTEMPTED, so it belongs in A and must be paid out of X. Falling out of both
// budgets in silence is the run axis's copy of "the absence of a day row is a
// recording defect".
//
// RecordPreflightRefusal is the one legal single-shot write, and only because
// two-phase is impossible for it: a refusal never invokes a process, so there
// is no "before the process" moment to trace, and preflight may have stopped
// before producing the columns a phase-1 row needs.
// ---------------------------------------------------------------------------

// OpenInvocation writes phase 1: the exec_invocation row with no verdict, plus
// the child rows preflight already produced, in ONE transaction under BEGIN
// IMMEDIATE.
//
// One transaction, not several. gateway.db learned this on the trigger log: a
// reservation and its event in two transactions means a lost record on the
// crash between them.
//
// There is no ON CONFLICT DO NOTHING. A duplicate invocation_id is a defect in
// whoever minted it, not an expected replay, and it comes back as
// ErrDuplicateInvocation.
func (db *DB) OpenInvocation(ctx context.Context, inv Invocation) error {
	if err := inv.Validate(); err != nil {
		return err
	}
	if inv.Preflight == nil {
		return fmt.Errorf("shadow: %s: OpenInvocation needs Preflight — the row is written after preflight and immediately before the process, so its outputs exist. A run that never got that far is RecordPreflightRefusal", inv.InvocationID)
	}
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		if err := insertOpen(ctx, tx, inv, nil); err != nil {
			return err
		}
		return insertOpenChildren(ctx, tx, inv, 0)
	})
}

// preflightRefusal is the closed half of a single-shot refusal row.
type preflightRefusal struct {
	reason     exec.RefusalReason
	finishedAt time.Time
}

// RecordPreflightRefusal writes a refusal that never reached the process, in
// one statement, closed.
//
// inv.Preflight MAY be nil, and that is the whole point of this call existing.
// The outcome vocabulary defines refusals — token_absent, normalisation_failed — that stop before
// argv or the normalised workflow text exist, and the sha of a text that does
// not exist cannot be written. Until 0002 the four columns were NOT NULL, so
// the schema required a refused row to carry values preflight never produced:
// these refusals may not have been recordable at all.
//
// It writes a CLOSED row directly rather than opening and closing one. A
// preflight refusal has no "during", so a phase-1 row for it would be a trace
// of a process that was never started.
func (db *DB) RecordPreflightRefusal(ctx context.Context, inv Invocation, reason exec.RefusalReason, finishedAt time.Time) error {
	if err := inv.Validate(); err != nil {
		return err
	}
	if !reason.Valid() {
		return fmt.Errorf("shadow: %s: refusal reason %q is outside the closed refusal set", inv.InvocationID, reason)
	}
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		if err := insertOpen(ctx, tx, inv, &preflightRefusal{reason: reason, finishedAt: finishedAt}); err != nil {
			return err
		}
		return insertOpenChildren(ctx, tx, inv, 0)
	})
}

// CloseInvocation writes phase 2: the five columns a closed row may acquire,
// plus the child rows that only exist after the run, in ONE transaction.
//
// The UPDATE names EXACTLY the five columns exec_invocation_write_once permits.
// Naming any other one is refused by the trigger even if the value does not
// change — UPDATE OF fires on a column being named, not on it changing — and
// that is deliberate: it means a writer cannot drift into rewriting `purpose`
// by copying a struct.
//
// The WHERE carries `AND outcome IS NULL`. That is a CAS, not decoration: it is
// what makes a lost race come back as ErrAlreadyClosed instead of overwriting a
// verdict. exec_invocation_no_reverdict holds the same line from the database
// side; both are kept, because the trigger produces a message and the CAS
// produces a distinguishable error.
func (db *DB) CloseInvocation(ctx context.Context, v Verdict) error {
	if err := v.Validate(); err != nil {
		return err
	}
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		var purpose string
		var closed sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT purpose, outcome FROM exec_invocation WHERE invocation_id = ?`,
			v.InvocationID).Scan(&purpose, &closed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s — phase 1 never ran, or the row was purged", ErrUnknownInvocation, v.InvocationID)
		case err != nil:
			return err
		case closed.Valid:
			return fmt.Errorf("%w: %s is already %s", ErrAlreadyClosed, v.InvocationID, closed.String)
		}

		// Asked here, before the CHECK does, so the caller gets a sentence
		// naming the gate rather than a constraint name.
		if PurposeComparisonRequired(exec.Purpose(purpose), v.Outcome) && v.Comparison == "" {
			return fmt.Errorf("%w: %s is a completed drill run and M is READ from this column, not derived", ErrComparisonRequired, v.InvocationID)
		}
		if v.Comparison != "" && exec.Purpose(purpose) != exec.PurposeDrill {
			return fmt.Errorf("%w: %s has purpose %q — there is no GitHub side to compare against", ErrComparisonForbidden, v.InvocationID, purpose)
		}

		res, err := tx.ExecContext(ctx, `
			UPDATE exec_invocation
			   SET finished_at    = ?,
			       outcome        = ?,
			       refusal_reason = ?,
			       run_conclusion = ?,
			       comparison     = ?
			 WHERE invocation_id = ? AND outcome IS NULL`,
			nullMillis(v.FinishedAt), string(v.Outcome),
			nullString(string(v.RefusalReason)), nullString(string(v.RunConclusion)),
			nullString(string(v.Comparison)), v.InvocationID)
		if err != nil {
			if extendedCode(err) == constraintTrigger {
				return fmt.Errorf("%w: %s: %v", ErrAlreadyClosed, v.InvocationID, err)
			}
			return fmt.Errorf("shadow: closing %s: %w", v.InvocationID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s (the CAS on outcome IS NULL matched nothing)", ErrAlreadyClosed, v.InvocationID)
		}

		for _, j := range v.Jobs {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO exec_job_result (invocation_id, job_id, leg_key, leg_resolved, source, conclusion)
				VALUES (?,?,?,?,?,?)`,
				v.InvocationID, j.JobID, j.Leg.String(), boolInt(j.Leg.Resolved()),
				string(j.Source), string(j.Conclusion),
			); err != nil {
				return fmt.Errorf("shadow: exec_job_result %s: %w", j.JobID, err)
			}
			for _, s := range j.Steps {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO exec_step_observation (invocation_id, job_id, leg_key, ordinal,
						step_id, step_name, ran, conclusion)
					VALUES (?,?,?,?,?,?,?,?)`,
					v.InvocationID, j.JobID, j.Leg.String(), s.Ordinal,
					s.CanonicalStepID(), s.Name, boolInt(s.Ran), nullString(string(s.Conclusion)),
				); err != nil {
					return fmt.Errorf("shadow: exec_step_observation %s/%s: %w", j.JobID, s.CanonicalStepID(), err)
				}
			}
		}

		for _, r := range v.Residue {
			observed := r.ObservedAt
			if observed.IsZero() {
				observed = v.FinishedAt
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO exec_residue (invocation_id, kind, ref, observed_at) VALUES (?,?,?,?)`,
				v.InvocationID, r.Kind, r.Ref, observed.UnixMilli(),
			); err != nil {
				return fmt.Errorf("shadow: exec_residue %s/%s: %w", r.Kind, r.Ref, err)
			}
		}

		// Phase 2's notes continue phase 1's numbering rather than restarting
		// it. Restarting would collide on the primary key, and a note dropped
		// by a collision is a finding nobody ever reads.
		var next int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM exec_note WHERE invocation_id = ?`,
			v.InvocationID).Scan(&next); err != nil {
			return err
		}
		return insertNotes(ctx, tx, v.InvocationID, v.Notes, next)
	})
}

// insertOpen writes the exec_invocation row. ref == nil writes an OPEN row;
// non-nil writes a closed single-shot preflight refusal.
//
// Columns are named, never positional. A positional INSERT turns a new column
// into a runtime bug instead of a compile error.
func insertOpen(ctx context.Context, tx *sql.Conn, inv Invocation, ref *preflightRefusal) error {
	var argv, normSHA, normApplied, workdirClean, commitCount, shallow any
	if p := inv.Preflight; p != nil {
		argv = p.ArgvSHA256
		normSHA = p.WorkflowNormSHA256
		normApplied = boolInt(p.WorkflowNormApplied)
		workdirClean = boolInt(p.Workdir.Clean)
		commitCount = p.Workdir.CommitCount
		shallow = nullBool(p.Workdir.Shallow)
	}
	var finished, outcome, reason any
	if ref != nil {
		finished = nullMillis(ref.finishedAt)
		outcome = string(exec.OutcomeRefused)
		reason = string(ref.reason)
	}

	// schema_version is NOT named. It takes the column DEFAULT, which 0002
	// moved to 2, and exec_invocation_schema_floor refuses anything lower —
	// a row claiming schema_version < 2 is exempt from the comparison
	// requirement, and only rows this build did NOT write may be.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO exec_invocation (
			invocation_id, backend_name, executor_kind, executor_version, executor_pin_ok,
			started_at, finished_at,
			repo, workflow_path, commit_sha, event_name, event_payload_sha256,
			purpose, drill_day, supply_mode,
			argv_sha256, workflow_norm_sha256, workflow_norm_applied,
			workdir_clean, workdir_commit_count, workdir_shallow,
			outcome, refusal_reason, run_conclusion, comparison
		) VALUES (?,?,?,?,?, ?,?, ?,?,?,?,?, ?,?,?, ?,?,?, ?,?,?, ?,?,NULL,NULL)`,
		inv.InvocationID, inv.BackendName, inv.Identity.Kind, inv.Identity.Version, boolInt(inv.Identity.VersionPinOK),
		inv.StartedAt.UnixMilli(), finished,
		inv.Repo, inv.WorkflowPath, inv.CommitSHA, inv.EventName, inv.EventPayloadSHA256,
		string(inv.Purpose), inv.DrillDay, string(inv.SupplyMode),
		argv, normSHA, normApplied,
		workdirClean, commitCount, shallow,
		outcome, reason,
	); err != nil {
		// Classified HERE, at the statement, not at the end of the
		// transaction. Doing it on the transaction's error mapped a UNIQUE
		// violation on the step-ordinal index to ErrDuplicateInvocation — a
		// legal-looking answer produced at the wrong call site — valid-looking
		// data with no external symptom — and is exactly what the drive caught.
		switch extendedCode(err) {
		case constraintPrimaryKey, constraintUnique:
			return fmt.Errorf("%w: %s: %v", ErrDuplicateInvocation, inv.InvocationID, err)
		case constraintForeignKey:
			// The only parent this row has that is not written in the same
			// transaction is drill_day.
			return fmt.Errorf("%w: %s (run %s)", ErrUnknownDay, inv.DrillDay, inv.InvocationID)
		}
		return fmt.Errorf("shadow: exec_invocation: %w", err)
	}
	return nil
}

// insertOpenChildren writes the child rows preflight produced: the image
// assertions, the local-repository revisions and the free notes.
func insertOpenChildren(ctx context.Context, tx *sql.Conn, inv Invocation, noteSeq int) error {
	for _, img := range inv.Images {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO exec_image (invocation_id, runs_on_label, local_tag, source_digest,
				architecture, config_nonempty, path_present)
			VALUES (?,?,?,?,?,?,?)`,
			inv.InvocationID, img.RunsOnLabel, img.LocalTag, img.SourceDigest,
			img.Architecture, boolInt(img.ConfigNonEmpty), boolInt(img.PathPresent),
		); err != nil {
			return fmt.Errorf("shadow: exec_image %s: %w", img.RunsOnLabel, err)
		}
	}
	for _, lr := range inv.LocalRepositories {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO exec_local_repository (invocation_id, url, local_path, resolved_sha)
			VALUES (?,?,?,?)`,
			inv.InvocationID, lr.URL, lr.LocalPath, lr.ResolvedSHA,
		); err != nil {
			return fmt.Errorf("shadow: exec_local_repository %s: %w", lr.URL, err)
		}
	}
	return insertNotes(ctx, tx, inv.InvocationID, inv.Notes, noteSeq)
}

func insertNotes(ctx context.Context, tx *sql.Conn, id string, notes []string, from int) error {
	for i, n := range notes {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO exec_note (invocation_id, seq, note) VALUES (?,?,?)`,
			id, from+i, n,
		); err != nil {
			return fmt.Errorf("shadow: exec_note %d: %w", from+i, err)
		}
	}
	return nil
}

// Notes reads the free findings recorded for one invocation, in emission order.
//
// NoteFrequency is the query the notes actually exist for — "anything
// appearing here repeatedly is a report to the maintainers, not a new
// constant". Repeatedly is a
// count, which is why the notes are rows rather than a joined TEXT column.
func (db *DB) Notes(ctx context.Context, invocationID string) ([]string, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT note FROM exec_note WHERE invocation_id = ? ORDER BY seq`, invocationID)
	if err != nil {
		return nil, fmt.Errorf("shadow: reading exec_note: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NoteCount is one distinct note text and how many invocations carried it.
type NoteCount struct {
	Note  string
	Count int
}

// NoteFrequency groups the free notes by text, most frequent first. A note that
// keeps recurring is a report to the maintainers: either the vocabulary is missing a
// name for something real, or the host keeps doing something the caller cannot
// see — the `.actrc` finding is exactly the second kind.
func (db *DB) NoteFrequency(ctx context.Context) ([]NoteCount, error) {
	rows, err := db.reader.QueryContext(ctx,
		`SELECT note, COUNT(*) AS n FROM exec_note GROUP BY note ORDER BY n DESC, note`)
	if err != nil {
		return nil, fmt.Errorf("shadow: grouping exec_note: %w", err)
	}
	defer rows.Close()
	var out []NoteCount
	for rows.Next() {
		var nc NoteCount
		if err := rows.Scan(&nc.Note, &nc.Count); err != nil {
			return nil, err
		}
		out = append(out, nc)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullBool(b *bool) any {
	if b == nil {
		return nil
	}
	return boolInt(*b)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullMillis maps the zero time.Time to NULL. NULL finished_at is the fifth outcome
// state — the process never finished — and it is reached with
// outcome = deadline_exceeded, never on its own.
func nullMillis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}
