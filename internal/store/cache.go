package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// reservedInputKey is the input the gateway injects for correlation.
// A caller that sends it gets 400 bad_request / reserved_input_key.
const reservedInputKey = "standby_trigger_id"

// hasInput reports whether the cached inputs definition declares key. The
// definition is the raw JSON of on.workflow_dispatch.inputs, an object keyed
// by input name.
func (e WorkflowCacheEntry) hasInput(key string) bool {
	if strings.TrimSpace(e.InputsDef) == "" {
		return false
	}
	var def map[string]json.RawMessage
	if err := json.Unmarshal([]byte(e.InputsDef), &def); err != nil {
		// An unparseable definition is treated as "not declared". Fail closed:
		// no injection, so the trigger event records correlation
		// 'dispatch_response' — or 'window' when layer 1 is off.
		return false
	}
	_, ok := def[key]
	return ok
}

// ReservedInputKey is exported so the gateway and the store agree on the name
// without duplicating the literal.
func ReservedInputKey() string { return reservedInputKey }

// UpsertWorkflowCache writes workflow definitions. The cache is
// persistent and keyed by (repo, workflow, ref), because workflow_dispatch
// runs the file at the given ref and different branches can declare different
// inputs.
func (db *DB) UpsertWorkflowCache(ctx context.Context, entries []WorkflowCacheEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		for _, e := range entries {
			if e.Repo == "" || e.Workflow == "" {
				return errors.New("store: workflow cache entry needs repo and workflow")
			}
			ref := e.Ref
			if ref == "" {
				ref = "default"
			}
			triggers, err := json.Marshal(e.Triggers)
			if err != nil {
				return err
			}
			fetched := e.FetchedAt
			if fetched.IsZero() {
				fetched = db.cfg.now()
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workflow_cache (repo, workflow, ref, workflow_id, triggers,
				                            inputs_def, default_branch, fetched_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(repo, workflow, ref) DO UPDATE SET
				    workflow_id    = excluded.workflow_id,
				    triggers       = excluded.triggers,
				    inputs_def     = excluded.inputs_def,
				    default_branch = excluded.default_branch,
				    fetched_at     = excluded.fetched_at`,
				e.Repo, e.Workflow, ref, e.WorkflowID, string(triggers),
				nullable(e.InputsDef), nullable(e.DefaultBranch), fetched.UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
}

const selectWorkflowCacheSQL = `
SELECT repo, workflow, ref, workflow_id, triggers, inputs_def, default_branch, fetched_at
  FROM workflow_cache`

// WorkflowDefinition reads one cached definition. ErrNotFound means the
// cache is COLD for this workflow, which is a degradation the caller must
// surface (gateway_workflow_cache_cold_total, inputs_validation
// 'skipped_cold_cache'), never hide.
//
// A stale entry — fetched_at older than the TTL — is still returned. The cache's
// stale-while-revalidate rule is explicit that only a never-fetched workflow
// counts as cold; the caller compares FetchedAt itself and refreshes in the
// background.
func (db *DB) WorkflowDefinition(ctx context.Context, repo, workflow, ref string) (WorkflowCacheEntry, error) {
	if ref == "" {
		ref = "default"
	}
	row := db.reader.QueryRowContext(ctx, selectWorkflowCacheSQL+
		` WHERE repo = ? AND workflow = ? AND ref = ?`, repo, workflow, ref)
	return scanWorkflowCache(row)
}

// WorkflowCacheAll returns every cached definition, for the inventory surface.
func (db *DB) WorkflowCacheAll(ctx context.Context) ([]WorkflowCacheEntry, error) {
	rows, err := db.reader.QueryContext(ctx, selectWorkflowCacheSQL+` ORDER BY repo, workflow, ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkflowCacheEntry
	for rows.Next() {
		e, err := scanWorkflowCache(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanWorkflowCache(row rowScanner) (WorkflowCacheEntry, error) {
	var (
		e         WorkflowCacheEntry
		triggers  string
		inputsDef sql.NullString
		defBranch sql.NullString
		fetchedMS int64
	)
	err := row.Scan(&e.Repo, &e.Workflow, &e.Ref, &e.WorkflowID, &triggers, &inputsDef, &defBranch, &fetchedMS)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if triggers != "" {
		if err := json.Unmarshal([]byte(triggers), &e.Triggers); err != nil {
			return e, fmt.Errorf("store: workflow_cache.triggers for %s/%s is not a JSON array: %w", e.Repo, e.Workflow, err)
		}
	}
	e.InputsDef = inputsDef.String
	e.DefaultBranch = defBranch.String
	e.FetchedAt = time.UnixMilli(fetchedMS)
	return e, nil
}

// NormalizeRef turns a GitHub head_branch into the gateway's full ref form:
// "main" -> "refs/heads/main". A value that already looks like a ref is
// returned unchanged.
func NormalizeRef(headBranch string) string {
	if headBranch == "" {
		return ""
	}
	if strings.HasPrefix(headBranch, "refs/") {
		return headBranch
	}
	return "refs/heads/" + headBranch
}

// UpsertGitHubRuns writes the run-history cache and reports which runs
// changed conclusion while carrying an active run_link. That set is exactly
// the trigger condition for a run_observed event: the caller writes them
// with AppendRunObserved.
//
// github_runs is a cache, not part of the append-only log, so an in-place
// update here does not violate the log's append-only rule.
func (db *DB) UpsertGitHubRuns(ctx context.Context, runs []GitHubRun) ([]ConclusionChange, error) {
	if len(runs) == 0 {
		return nil, nil
	}
	var changes []ConclusionChange
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		changes = changes[:0]
		for _, r := range runs {
			if r.RunID == 0 {
				return errors.New("store: github run needs a run_id")
			}
			var prev sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT conclusion FROM github_runs WHERE run_id = ?`, r.RunID).Scan(&prev)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			normalized := r.HeadBranchNormalized
			if normalized == "" {
				normalized = NormalizeRef(r.HeadBranch)
			}
			fetched := r.FetchedAt
			if fetched.IsZero() {
				fetched = db.cfg.now()
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO github_runs (run_id, repo, workflow_id, workflow, event,
				                         head_branch, head_branch_normalized, actor,
				                         triggering_actor, conclusion, created_at, fetched_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(run_id) DO UPDATE SET
				    conclusion = excluded.conclusion,
				    fetched_at = excluded.fetched_at`,
				r.RunID, r.Repo, r.WorkflowID, r.Workflow, r.Event,
				nullable(r.HeadBranch), nullable(normalized), nullable(r.Actor),
				nullable(r.TriggeringActor), nullable(r.Conclusion),
				r.CreatedAt.UnixMilli(), fetched.UnixMilli()); err != nil {
				return err
			}

			// Only a real change, and only once a
			// conclusion exists.
			if r.Conclusion == "" || r.Conclusion == prev.String {
				continue
			}
			var triggerEventID string
			err = tx.QueryRowContext(ctx, `
				SELECT trigger_event_id FROM trigger_events
				 WHERE event_type = 'run_link' AND superseded_by IS NULL AND run_id = ?`,
				r.RunID).Scan(&triggerEventID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return err
			}
			changes = append(changes, ConclusionChange{
				RunID:          r.RunID,
				TriggerEventID: triggerEventID,
				From:           prev.String,
				To:             r.Conclusion,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return changes, nil
}

// ActiveRunLink returns the trigger_event_id of the active run_link for a run,
// or ErrNotFound. The resolver uses it to decide whether a freshly written
// run_link should immediately produce a run_observed: when the run's cached
// conclusion is already non-NULL it is written right away instead of waiting
// for the next inventory refresh.
func (db *DB) ActiveRunLink(ctx context.Context, runID int64) (string, error) {
	var id string
	err := db.reader.QueryRowContext(ctx, `
		SELECT trigger_event_id FROM trigger_events
		 WHERE event_type = 'run_link' AND superseded_by IS NULL AND run_id = ?`, runID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// GitHubRunConclusion reads a cached run's conclusion, empty when the run has
// not finished.
func (db *DB) GitHubRunConclusion(ctx context.Context, runID int64) (string, error) {
	var c sql.NullString
	err := db.reader.QueryRowContext(ctx, `SELECT conclusion FROM github_runs WHERE run_id = ?`, runID).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return c.String, err
}
