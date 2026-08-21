package cli

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/httpapi"
	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
)

// background owns every loop that is not a request.
//
// All of them start AFTER the listeners are open and stop BEFORE the
// process exits but AFTER the listeners close — the maintenance sweep and the
// resolver are what clean up behind an interrupted dispatch, so shutting them
// down first would strand exactly the requests a shutdown creates.
type background struct {
	log      *slog.Logger
	db       *store.DB
	gh       *forge.Client
	srv      *httpapi.Server
	reg      *metrics.Registry
	reloader *config.Reloader
	cfg      *config.Config
}

func (b *background) start(ctx context.Context, wg *sync.WaitGroup) {
	run := func(name string, fn func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
			b.log.Debug("background loop stopped", "loop", name)
		}()
	}

	run("bootstrap", b.bootstrap)
	run("resolver", b.resolver)
	run("maintenance", b.maintenance)
	run("inventory-refresh", b.inventoryRefresh)
}

// bootstrap is the startup inventory discovery: workflow surface and run
// history.
//
// A failure here NEVER blocks startup and never stops the gateway: the whole
// product exists to work while GitHub is broken, so a gateway that refused to
// serve because bootstrap failed would fail in exactly the scenario it was
// built for. The result is logged and shows on gateway_target_unavailable.
func (b *background) bootstrap(ctx context.Context) {
	if !b.cfg.Inventory.Bootstrap() {
		b.log.Info("inventory bootstrap disabled by configuration")
		return
	}
	b.runInventory(ctx, "bootstrap")
}

// inventoryRefresh re-runs discovery on inventory.refresh_interval.
func (b *background) inventoryRefresh(ctx context.Context) {
	interval := b.cfg.Inventory.RefreshInterval.D()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.runInventory(ctx, "refresh")
		}
	}
}

func (b *background) runInventory(ctx context.Context, phase string) {
	repos := targetRepos(b.reloader.Current())
	if len(repos) == 0 {
		return
	}
	started := time.Now()
	results := b.gh.Bootstrap(ctx, repos)
	if ctx.Err() != nil {
		return
	}

	for _, res := range results {
		if res.Err != nil {
			// Not fatal, and not hidden. gateway_target_unavailable is set by
			// internal/forge on the same failure.
			b.log.Warn("inventory discovery failed for repo",
				"phase", phase, "repo", res.Repo.String(), "err", res.Err)
		}
		if res.History != nil {
			if res.History.Truncated {
				// An under-reported history understates the coverage
				// gap, so truncation is surfaced rather than swallowed.
				b.log.Warn("run history truncated by GitHub's result ceiling",
					"repo", res.Repo.String(), "days", res.History.TruncatedDays)
			}
			b.emitRunObserved(ctx, res.History.Changed)
		}
	}

	// The two coverage gauges. They are computed from the store rather
	// than accumulated, and they are NEVER summed with each other.
	rows, err := b.db.Coverage(ctx, b.cfg.Inventory.HistoryDays)
	if err != nil {
		b.log.Warn("coverage query failed", "err", err)
		return
	}
	summary := store.Summarise(rows)
	b.srv.PublishCoverage(summary)
	b.log.Info("inventory pass complete",
		"phase", phase, "repos", len(repos), "took", time.Since(started).String(),
		"coverage_gap_pct", summary.GapPct, "coverage_unknown_pct", summary.UnknownPct)
}

// emitRunObserved is the run_observed producer: a run whose conclusion changed
// and that has an active run_link becomes a run_observed event.
//
// The trigger id comes from ActiveRunLink rather than from the upsert result,
// because forge.RunRecord does not carry one — internal/forge does not know
// about trigger ids, and it should not.
func (b *background) emitRunObserved(ctx context.Context, changed []forge.RunRecord) {
	for _, run := range changed {
		if run.Conclusion == "" {
			// `in_progress` is a status, never a conclusion. A run with
			// no conclusion has not finished and produces no observation.
			continue
		}
		triggerID, err := b.db.ActiveRunLink(ctx, run.RunID)
		if errors.Is(err, store.ErrNotFound) || triggerID == "" {
			// A run nobody triggered through the gateway. It belongs to the
			// coverage gap, not to the event log.
			continue
		}
		if err != nil {
			b.log.Warn("active run_link lookup failed", "run_id", run.RunID, "err", err)
			continue
		}
		if _, err := b.db.AppendRunObserved(ctx, store.RunObservedEvent{
			TS:             time.Now(),
			TriggerEventID: triggerID,
			RunID:          run.RunID,
			Conclusion:     run.Conclusion,
		}); err != nil {
			b.log.Warn("run_observed not written", "run_id", run.RunID, "err", err)
		}
	}
}

// resolver runs the tier 2/3 correlation sweep.
//
// It is query-based: its whole state is the database, so a restart resumes
// without losing a pending correlation. Tier 1 links written on the request
// path drop their triggers out of the query entirely, so in a healthy
// deployment this loop finds almost nothing — which is the point.
func (b *background) resolver(ctx context.Context) {
	if err := forge.NewResolver(b.gh).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		b.log.Error("correlation resolver stopped", "err", err)
	}
}

// sweepInterval is how often the crash-recovery sweep runs. SweepParams.Margin
// must comfortably exceed it, or a row can cross the threshold and be swept in
// the same tick as a dispatch that is still legitimately running.
const sweepInterval = 30 * time.Second

// maintenance runs the idempotency purge, the crash-recovery sweep and event
// retention.
func (b *background) maintenance(ctx context.Context) {
	sweep := time.NewTicker(sweepInterval)
	defer sweep.Stop()
	purge := time.NewTicker(store.IdempotencyPurgePeriod)
	defer purge.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			n, err := b.db.SweepStaleInFlight(ctx, store.SweepParams{
				UpstreamTimeout: b.cfg.Gateway.UpstreamTimeout.D(),
				MaxQueueWait:    b.cfg.GitHub.MaxQueueWait.D(),
				Margin:          4 * sweepInterval,
			})
			if err != nil {
				b.log.Warn("in-flight sweep failed", "err", err)
			} else if n > 0 {
				b.log.Info("in-flight rows swept to unknown", "rows", n)
			}
		case <-purge.C:
			b.purge(ctx)
		}
	}
}

func (b *background) purge(ctx context.Context) {
	n, err := b.db.PurgeExpiredIdempotency(ctx)
	switch {
	case errors.Is(err, store.ErrClockJumpSuspected):
		// Nothing was deleted, on purpose. The counter and a warn
		// line are the whole point — the risk this guard narrows is a SILENT
		// double dispatch, and a false positive after a long maintenance stop
		// is expected, so this must not drive an alert on its own.
		b.reg.IncCounter("gateway_clock_jump_suspected_total", nil)
		b.log.Warn("idempotency purge skipped: forward clock jump suspected",
			"total", b.db.ClockJumpSuspectedTotal())
	case err != nil:
		b.log.Warn("idempotency purge failed", "err", err)
	case n > 0:
		b.log.Info("expired idempotency rows purged", "rows", n)
	}

	retention := time.Duration(b.cfg.Gateway.RetentionDays) * 24 * time.Hour
	if events, err := b.db.PurgeOldEvents(ctx, retention); err != nil {
		b.log.Warn("event retention purge failed", "err", err)
	} else if events > 0 {
		b.log.Info("old events purged", "rows", events, "retention_days", b.cfg.Gateway.RetentionDays)
	}
	if runs, err := b.db.PurgeOldRuns(ctx, retention); err != nil {
		b.log.Warn("run cache purge failed", "err", err)
	} else if runs > 0 {
		b.log.Info("old run cache rows purged", "rows", runs)
	}
}

// targetRepos is the deduplicated repo list from allowed_targets. It is read
// from the LIVE configuration so a SIGHUP that adds a repo is picked up by the
// next refresh without a restart (SIGHUP reloads allowed_targets).
func targetRepos(cfg *config.Config) []forge.Repo {
	seen := map[string]bool{}
	out := make([]forge.Repo, 0, len(cfg.AllowedTargets))
	for _, t := range cfg.AllowedTargets {
		if seen[t.Repo] {
			continue
		}
		repo, err := forge.ParseRepo(t.Repo)
		if err != nil {
			continue
		}
		seen[t.Repo] = true
		out = append(out, repo)
	}
	return out
}
