package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// UnresolvedTrigger is one forwarded trigger whose correlation is not yet
// settled: it has no active `exact` run_link.
type UnresolvedTrigger struct {
	EventID        string
	TargetProject  string
	TargetWorkflow string
	TargetRef      string
	TS             time.Time

	// Correlation is what this dispatch was ARMED with. The resolver
	// branches on it: only an `injected` trigger can be resolved by layer 2
	// (standby_trigger_id in display_title), everything else goes straight to
	// layer 3's time window. Without this field that branch can never be true
	// and the recovery path is dead code.
	Correlation Correlation

	// ExistingResolution is the active run_link's resolution, empty when the
	// trigger has no link yet. It is what makes upgrading a weak link possible: a trigger with an
	// `inferred` or `ambiguous` link is still in this queue and may be upgraded,
	// and the resolver needs to know what it has to beat.
	//
	// It is an OPTIMISATION only. Two concurrent sweeps can both read the same
	// value and both decide to write; the supersede rule inside the store is
	// the authority.
	ExistingResolution Resolution
}

// unresolvedTriggersSQL is the background resolver's query, in its revised
// form.
//
// Two points where it differs from the original query:
//
//  1. `outcome_status = 'forwarded'` is an EXISTS against the trigger's
//     trigger_outcome event. A trigger row's own outcome_status is
//     structurally NULL (the result lives in trigger_outcome), so the literal filter
//     matched nothing, forever, silently.
//  2. The exclusion is `NOT EXISTS ... AND rl.resolution = 'exact'`. Only
//     an `exact` link ends the search: a `none`, `ambiguous` or `inferred` link
//     is active but weak, and the old "has any active link" form meant the
//     supersede ladder could never run. The NOT EXISTS form is also immune to
//     NOT IN's NULL trap by construction (a single NULL in the subquery makes
//     NOT IN match nothing), so the
//     `trigger_event_id IS NOT NULL` patch this file used to carry is gone.
//
// The LEFT JOIN is this package's addition, for ExistingResolution. It
// cannot multiply rows: idx_run_link_unique allows at
// most one active run_link per trigger_event_id.
const unresolvedTriggersSQL = `
SELECT te.event_id, te.target_project, te.target_workflow, te.target_ref, te.ts,
       te.correlation, rl.resolution
  FROM trigger_events te
  LEFT JOIN trigger_events rl
    ON rl.event_type = 'run_link'
   AND rl.superseded_by IS NULL
   AND rl.trigger_event_id = te.event_id
 WHERE te.event_type = 'trigger'
   AND EXISTS (
     SELECT 1 FROM trigger_events o
      WHERE o.event_type = 'trigger_outcome'
        AND o.trigger_event_id = te.event_id
        AND o.outcome_status = 'forwarded'
   )
   AND NOT EXISTS (
     SELECT 1 FROM trigger_events x
      WHERE x.event_type = 'run_link'
        AND x.superseded_by IS NULL
        AND x.trigger_event_id = te.event_id
        AND x.resolution = 'exact'
   )
   AND te.ts > ?
 ORDER BY te.ts
 LIMIT ?`

// UnresolvedTriggers returns forwarded triggers with no active `exact`
// run_link. The resolver is query-driven rather than timer-driven
// precisely so it resumes by itself after a restart: there is no in-memory
// queue to lose.
//
// A layer-1 trigger never appears here: the request path writes its link with
// resolution `exact` right after the dispatch, and the NOT EXISTS drops it.
// That is the mechanism that removes the per-trigger GitHub lookup, which was
// otherwise the dominant cost against the API quota.
//
// horizon is resolver_horizon (default 5 minutes). Triggers older than that
// are the resolver's cue to write resolution 'none'; they stop appearing here
// once it does.
func (db *DB) UnresolvedTriggers(ctx context.Context, horizon time.Duration, limit int) ([]UnresolvedTrigger, error) {
	if limit <= 0 {
		limit = 500
	}
	cutoff := db.cfg.now().Add(-horizon).UnixMilli()
	rows, err := db.reader.QueryContext(ctx, unresolvedTriggersSQL, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UnresolvedTrigger
	for rows.Next() {
		var (
			t                       UnresolvedTrigger
			project, workflow, ref  sql.NullString
			correlation, resolution sql.NullString
			ts                      int64
		)
		if err := rows.Scan(&t.EventID, &project, &workflow, &ref, &ts, &correlation, &resolution); err != nil {
			return nil, err
		}
		t.TargetProject, t.TargetWorkflow, t.TargetRef = project.String, workflow.String, ref.String
		t.TS = time.UnixMilli(ts)
		t.Correlation = Correlation(correlation.String)
		t.ExistingResolution = Resolution(resolution.String)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Coverage states.
const (
	CoverageCovered = "covered"
	CoverageUnknown = "unknown"
	CoverageGap     = "gap"
)

// Gap classes. The gap does not converge to zero and is not
// expected to.
const (
	GapMigratable = "migratable"
	GapStructural = "structural"
)

// CoverageRow is one row of the coverage gap report.
type CoverageRow struct {
	Repo          string
	Workflow      string
	CoverageState string
	GapClass      string // only meaningful when CoverageState is "gap"
	RunCount      int64
	LastSeen      time.Time
}

// coverageSQL is the specified coverage gap query, with the one substitution the resolver
// query needs too: `te.outcome_status = 'forwarded'` on the trigger row cannot
// match (the outcome lives in its own event), so it is expressed as an EXISTS against the trigger's
// trigger_outcome event. Everything else — the CASE ladder, the ±60s join
// window, the GROUP BY, the ORDER BY — is as written.
const coverageSQL = `
SELECT
    gr.repo,
    gr.workflow,
    CASE
        WHEN rl.resolution = 'exact' THEN 'covered'
        WHEN rl.resolution IN ('inferred', 'ambiguous') THEN 'unknown'
        WHEN rl.resolution = 'none' THEN 'unknown'
        WHEN te.event_id IS NOT NULL THEN 'unknown'
        ELSE 'gap'
    END AS coverage_state,
    CASE
        WHEN rl.resolution IS NULL AND te.event_id IS NULL THEN
            CASE WHEN gr.event = 'workflow_dispatch' THEN 'migratable'
                 ELSE 'structural' END
        ELSE NULL
    END AS gap_class,
    COUNT(DISTINCT gr.run_id) AS run_count,
    MAX(gr.created_at) AS last_seen
FROM github_runs gr
LEFT JOIN trigger_events te
    ON te.event_type = 'trigger'
    AND EXISTS (
        SELECT 1 FROM trigger_events o
         WHERE o.event_type = 'trigger_outcome'
           AND o.trigger_event_id = te.event_id
           AND o.outcome_status = 'forwarded'
    )
    AND te.target_project = gr.repo
    AND te.target_workflow = gr.workflow
    AND te.ts BETWEEN gr.created_at - 60000 AND gr.created_at + 60000
LEFT JOIN trigger_events rl
    ON rl.event_type = 'run_link'
    AND rl.superseded_by IS NULL
    AND rl.run_id = gr.run_id
WHERE gr.event IN ('workflow_dispatch', 'repository_dispatch')
  AND gr.created_at > ?
GROUP BY gr.repo, gr.workflow, coverage_state, gap_class
ORDER BY run_count DESC`

// Coverage runs the coverage gap query. historyDays comes from
// inventory.history_days / gateway.retention_days and is never hard
// coded.
//
// It runs on the read pool. It is a full scan of the run cache joined twice
// against the event log; on the single writer it would sit in front of the
// next dispatch.
func (db *DB) Coverage(ctx context.Context, historyDays int) ([]CoverageRow, error) {
	if historyDays <= 0 {
		return nil, errors.New("store: Coverage needs a positive historyDays")
	}
	cutoff := db.cfg.now().Add(-time.Duration(historyDays) * 24 * time.Hour).UnixMilli()
	rows, err := db.reader.QueryContext(ctx, coverageSQL, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CoverageRow
	for rows.Next() {
		var (
			r        CoverageRow
			gapClass sql.NullString
			lastSeen sql.NullInt64
		)
		if err := rows.Scan(&r.Repo, &r.Workflow, &r.CoverageState, &gapClass, &r.RunCount, &lastSeen); err != nil {
			return nil, err
		}
		r.GapClass = gapClass.String
		if lastSeen.Valid {
			r.LastSeen = time.UnixMilli(lastSeen.Int64)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CoverageSummary is the counter/percentage block of the inventory report. The two
// percentages are separate gauges and are never added together.
type CoverageSummary struct {
	Total        int64
	CoveredCount int64
	UnknownCount int64
	GapCount     int64
	GapPct       float64
	UnknownPct   float64
}

// Summarise reduces coverage rows to the inventory counters. Percentages are over
// run_count, not over row count: one workflow triggered a thousand times
// outside the gateway is a bigger gap than one triggered twice.
func Summarise(rows []CoverageRow) CoverageSummary {
	var s CoverageSummary
	for _, r := range rows {
		s.Total += r.RunCount
		switch r.CoverageState {
		case CoverageCovered:
			s.CoveredCount += r.RunCount
		case CoverageUnknown:
			s.UnknownCount += r.RunCount
		case CoverageGap:
			s.GapCount += r.RunCount
		}
	}
	if s.Total > 0 {
		s.GapPct = float64(s.GapCount) * 100 / float64(s.Total)
		s.UnknownPct = float64(s.UnknownCount) * 100 / float64(s.Total)
	}
	return s
}

// ObservedRow is one entry of the inventory `observed` list.
type ObservedRow struct {
	GitHubActor   string
	Caller        string
	CallerTokenID string
	Repo          string
	Workflow      string
	WeeklyCount   int64
	LastSeen      time.Time
	Backend       string
	Coverage      string
}

// observedFromRunsSQL is the run-history side of the `observed` list. The
// inventory output has a fixed shape but no specified SQL, so this is derived: it reuses the coverage query's
// join and CASE ladder and adds the actor and caller columns the JSON needs.
const observedFromRunsSQL = `
SELECT
    COALESCE(gr.triggering_actor, gr.actor, '') AS github_actor,
    COALESCE(te.caller_name, '')                AS caller,
    COALESCE(te.caller_token_id, '')            AS caller_token,
    gr.repo,
    gr.workflow,
    CASE
        WHEN rl.resolution = 'exact' THEN 'covered'
        WHEN rl.resolution IN ('inferred', 'ambiguous', 'none') THEN 'unknown'
        WHEN te.event_id IS NOT NULL THEN 'unknown'
        ELSE 'gap'
    END AS coverage_state,
    COALESCE(te.decision_backend, '')            AS backend,
    COUNT(DISTINCT CASE WHEN gr.created_at > ? THEN gr.run_id END) AS weekly_count,
    MAX(gr.created_at) AS last_seen
FROM github_runs gr
LEFT JOIN trigger_events te
    ON te.event_type = 'trigger'
    AND EXISTS (
        SELECT 1 FROM trigger_events o
         WHERE o.event_type = 'trigger_outcome'
           AND o.trigger_event_id = te.event_id
           AND o.outcome_status = 'forwarded'
    )
    AND te.target_project = gr.repo
    AND te.target_workflow = gr.workflow
    AND te.ts BETWEEN gr.created_at - 60000 AND gr.created_at + 60000
LEFT JOIN trigger_events rl
    ON rl.event_type = 'run_link'
    AND rl.superseded_by IS NULL
    AND rl.run_id = gr.run_id
WHERE gr.event IN ('workflow_dispatch', 'repository_dispatch')
  AND gr.created_at > ?
GROUP BY github_actor, caller, caller_token, gr.repo, gr.workflow, coverage_state, backend
ORDER BY weekly_count DESC`

// observedGatewayOnlySQL covers the gateway-only case: a caller went
// through the gateway but no GitHub run was ever matched to it. Those triggers
// are invisible to the run-history side of the query, and dropping them would
// under-report the unknown class — the one class that must never be hidden.
const observedGatewayOnlySQL = `
SELECT
    COALESCE(te.caller_name, '')     AS caller,
    COALESCE(te.caller_token_id, '') AS caller_token,
    COALESCE(te.target_project, '')  AS repo_name,
    COALESCE(te.target_workflow, '') AS workflow_name,
    COALESCE(te.decision_backend, '') AS backend,
    COUNT(*) FILTER (WHERE te.ts > ?) AS weekly_count,
    MAX(te.ts) AS last_seen
FROM trigger_events te
WHERE te.event_type = 'trigger'
  AND te.ts > ?
  AND EXISTS (
      SELECT 1 FROM trigger_events o
       WHERE o.event_type = 'trigger_outcome'
         AND o.trigger_event_id = te.event_id
         AND o.outcome_status = 'forwarded'
  )
  AND NOT EXISTS (
      SELECT 1 FROM github_runs gr
       WHERE gr.repo = te.target_project
         AND gr.workflow = te.target_workflow
         AND te.ts BETWEEN gr.created_at - 60000 AND gr.created_at + 60000
  )
GROUP BY caller, caller_token, repo_name, workflow_name, backend
ORDER BY weekly_count DESC`

// Observed builds the inventory `observed` list from both sides: runs
// seen on GitHub, and gateway triggers that no run was ever matched to.
func (db *DB) Observed(ctx context.Context, historyDays int) ([]ObservedRow, error) {
	if historyDays <= 0 {
		return nil, errors.New("store: Observed needs a positive historyDays")
	}
	now := db.cfg.now()
	windowCutoff := now.Add(-time.Duration(historyDays) * 24 * time.Hour).UnixMilli()
	weekCutoff := now.Add(-7 * 24 * time.Hour).UnixMilli()

	rows, err := db.reader.QueryContext(ctx, observedFromRunsSQL, weekCutoff, windowCutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ObservedRow
	for rows.Next() {
		var (
			r        ObservedRow
			lastSeen sql.NullInt64
		)
		if err := rows.Scan(&r.GitHubActor, &r.Caller, &r.CallerTokenID, &r.Repo, &r.Workflow,
			&r.Coverage, &r.Backend, &r.WeeklyCount, &lastSeen); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			r.LastSeen = time.UnixMilli(lastSeen.Int64)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	gwRows, err := db.reader.QueryContext(ctx, observedGatewayOnlySQL, weekCutoff, windowCutoff)
	if err != nil {
		return nil, err
	}
	defer gwRows.Close()
	for gwRows.Next() {
		var (
			r        ObservedRow
			lastSeen sql.NullInt64
		)
		if err := gwRows.Scan(&r.Caller, &r.CallerTokenID, &r.Repo, &r.Workflow, &r.Backend,
			&r.WeeklyCount, &lastSeen); err != nil {
			return nil, err
		}
		if lastSeen.Valid {
			r.LastSeen = time.UnixMilli(lastSeen.Int64)
		}
		// No GitHub run was matched: the gateway forwarded it and cannot prove
		// what it became.
		r.Coverage = CoverageUnknown
		out = append(out, r)
	}
	return out, gwRows.Err()
}

// SurfaceRow is one entry of the inventory `surface` list.
type SurfaceRow struct {
	Repo     string
	Workflow string
	Triggers []string
	// CorrelationReady: standby_trigger_id is declared in the
	// cached inputs definition, so the run-name correlation path is
	// available for this workflow.
	CorrelationReady bool
	// FetchedAt is when the definition was last fetched. A zero value cannot
	// occur here — a workflow with no cache row does not appear in the surface
	// at all, which is what "cold" means.
	FetchedAt time.Time
}

// Surface lists the cached workflow definitions. Phase 0 caches only
// the default ref, so entries are deduplicated on (repo, workflow) with the
// default ref winning.
func (db *DB) Surface(ctx context.Context) ([]SurfaceRow, error) {
	entries, err := db.WorkflowCacheAll(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]int, len(entries))
	var out []SurfaceRow
	for _, e := range entries {
		key := e.Repo + "\x00" + e.Workflow
		row := SurfaceRow{
			Repo:             e.Repo,
			Workflow:         e.Workflow,
			Triggers:         e.Triggers,
			CorrelationReady: e.CorrelationReady(),
			FetchedAt:        e.FetchedAt,
		}
		if idx, ok := seen[key]; ok {
			if e.Ref == "default" {
				out[idx] = row
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, row)
	}
	return out, nil
}
