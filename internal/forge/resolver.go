package forge

import (
	"context"
	"errors"
	"time"

	"github.com/google/go-github/v90/github"
)

// Resolver establishes the run_id ↔ trigger_event_id link (the correlation
// hierarchy, see docs/architecture.md).
//
// It is QUERY-BASED, not timer-based. The ticker below is only a wake-up: the
// set of work is re-derived from the database on every sweep by
// CorrelationStore.PendingCorrelations. Nothing pending is held in memory, so
// a restart resumes exactly where it left off and no correlation is lost.
type Resolver struct {
	c *Client
}

// NewResolver builds the resolver. It shares the client's limiter, so its
// queries compete for the same in-flight budget and pause with the same
// per-installation quota rule.
func NewResolver(c *Client) *Resolver { return &Resolver{c: c} }

// SweepStats is one sweep's outcome.
type SweepStats struct {
	Pending   int
	Exact     int
	Inferred  int
	Ambiguous int
	None      int
	Deferred  int
	Errors    int
	// RunNameAbsent counts triggers that reached the horizon with tier 2
	// ARMED, no run carrying the trigger id, and at least one run that the
	// time window and ref accept as this trigger's. See tier2Outcome: this is
	// the only shape in which "the run-name did not carry what we injected"
	// leaves a trace. It is a subset of Inferred+Ambiguous, never a summand.
	RunNameAbsent int
}

// tier2Outcome is what the run-name search did on one pending trigger.
//
// It exists because tier 2 failing has NO natural symptom. A run whose
// run-name does not carry the injected id is indistinguishable, at the
// resolver's level, from a run that has not been created yet: both are "the
// loop finished without a match". The trigger then falls to tier 3, is
// recorded `inferred` or `ambiguous`, and no error is raised anywhere —
// which is a silent degradation in its purest form.
//
// Measured on 2026-08-21 to be a real case and not a hypothetical: GitHub
// does not always apply a `run-name:` that the workflow file declares at the
// run's own head_sha (runname.go).
//
// THIS DOES NOT GO AWAY NOW THAT THE RUN-NAME SEAM IS VERIFIED, and deleting
// it as "answered" would be the mistake this paragraph exists to prevent. The
// same day's probe measured that `workflow_dispatch` runs DO carry the
// run-name in both fields — but the not-applied case was observed on
// `pull_request`, application is known to vary BY EVENT TYPE, and the probe
// was one repo, one workflow file, four calls, one day. If application ever
// starts varying on `workflow_dispatch` too, there is no error anywhere and a
// shift in this counter's rate is the ONLY trace it would leave.
//
// WHAT THIS CAN AND CANNOT SEPARATE — read before trusting the counter:
//
//   - It CAN separate "there was nothing to search" from "there was something
//     to search and it did not carry the id". A trigger past its horizon whose
//     window holds a run on the right ref, where no run carries the id, is
//     positive evidence that the marker is missing rather than late. That
//     comparison is available inside a single lookup and costs nothing: the
//     window match is computed one branch later regardless.
//   - It CANNOT say WHY the marker is missing. GitHub not applying a declared
//     `run-name`, an operator whose `run-name:` does not interpolate
//     standby_trigger_id, an operator who removed the line, and a window
//     candidate that is somebody else's dispatch entirely all produce exactly
//     this observation. One occurrence does not choose between them.
//   - It CANNOT be read from one trigger. The diagnosis is a RATE: a
//     repo/workflow whose armed triggers keep landing here while tier 3 keeps
//     finding candidates is a workflow whose runs never carry the id. That is
//     why the counter is labelled by repo and workflow, and why it is a
//     counter rather than a field on one link.
type tier2Outcome int

const (
	// tier2NotArmed: correlation was not "injected"; no search was possible.
	tier2NotArmed tier2Outcome = iota
	// tier2Matched: a run carried the trigger id. Tier 2 proved the link.
	tier2Matched
	// tier2Pending: armed, searched, no match, still inside the horizon. The
	// run may simply not exist yet, so nothing is claimed and nothing is
	// counted.
	tier2Pending
	// tier2NoCandidate: armed, searched to the horizon, and the window held
	// no run this trigger could own. "Not there" — not evidence about the
	// run-name. Already visible as a `none` or a deferred link.
	tier2NoCandidate
	// tier2Absent: armed, searched to the horizon, and the window DID hold a
	// run on a matching ref that does not carry the id. This is the case that
	// had no symptom.
	tier2Absent
)

// sweepResolution is resolveOne's answer. It is a struct rather than four
// return values because tier2 is diagnostic output that must not be confused
// with the link itself at a call site.
type sweepResolution struct {
	link RunLink
	run  *github.WorkflowRun
	// write is false for "no answer yet, leave it pending".
	write bool
	tier2 tier2Outcome
}

// Run sweeps until ctx is done.
func (r *Resolver) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.c.cfg.ResolverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// A sweep error is logged by the caller and the next tick retries
			// from the database. There is no in-memory backlog to corrupt.
			if _, err := r.Sweep(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.c.metrics.IncCounter("gateway_correlation_sweep_error_total", nil)
			}
		}
	}
}

// Sweep resolves one batch of pending triggers.
func (r *Resolver) Sweep(ctx context.Context) (SweepStats, error) {
	var stats SweepStats
	if r.c.store == nil {
		return stats, errors.New("forge: no store configured")
	}

	pending, err := r.c.store.PendingCorrelations(ctx, r.c.cfg.ResolverHorizon, r.c.cfg.ResolverBatch)
	if err != nil {
		return stats, err
	}
	stats.Pending = len(pending)

	for _, p := range pending {
		res, err := r.resolveOne(ctx, p)
		if err != nil {
			stats.Errors++
			// One unreachable repo must not stop the batch: the others are
			// independent and their evidence is still perishable.
			continue
		}
		link, run := res.link, res.run

		// Recorded BEFORE the write branches below. The observation is about
		// the run-name, not about the link: a supersession that declines to
		// write, or a store error, does not make the missing marker any less
		// true, and dropping the count there would hide the degradation
		// exactly on the runs where it matters most.
		if res.tier2 == tier2Absent {
			stats.RunNameAbsent++
			r.c.metrics.IncCounter("gateway_run_name_absent_total", map[string]string{
				"repo":     p.Repo,
				"workflow": p.Workflow,
			})
		}

		if !res.write {
			stats.Deferred++
			continue
		}
		if p.ExistingResolution != "" && !link.Resolution.StrongerThan(p.ExistingResolution) {
			// A weaker resolution never supersedes a stronger one.
			stats.Deferred++
			continue
		}
		if err := r.c.store.WriteRunLink(ctx, link); err != nil {
			stats.Errors++
			continue
		}
		// gateway_run_link_total is labelled by BOTH resolution and
		// resolution_source, because tiers 1 and 2 both produce `exact` and the
		// operator checklist's adoption report reads the split. gateway_correlation_total is a
		// different counter, labelled by `correlation`, and is emitted on the
		// dispatch path — armament, not result.
		r.c.metrics.IncCounter("gateway_run_link_total", map[string]string{
			"resolution":        string(link.Resolution),
			"resolution_source": string(link.ResolutionSource),
		})

		switch link.Resolution {
		case ResolutionExact:
			stats.Exact++
		case ResolutionInferred:
			stats.Inferred++
		case ResolutionAmbiguous:
			stats.Ambiguous++
		default:
			stats.None++
		}

		// If the matched run already concluded, record it now rather
		// than waiting for the next inventory refresh.
		if run != nil && run.GetConclusion() != "" {
			_ = r.c.store.WriteRunObserved(ctx, RunObserved{
				TriggerEventID: p.EventID,
				RunID:          run.GetID(),
				Conclusion:     run.GetConclusion(),
				At:             r.c.now(),
			})
		}
	}
	return stats, nil
}

// resolveOne resolves a single pending trigger.
//
// sweepResolution.write=false means "no answer yet, leave it pending" — the
// trigger is still inside its horizon and a later sweep may find the run.
// Writing `none` early would replace a recoverable state with a permanent one.
//
// sweepResolution.tier2 is diagnostic and never decides anything here; it is
// what the caller counts. See tier2Outcome for what it can and cannot tell an
// operator.
func (r *Resolver) resolveOne(ctx context.Context, p PendingTrigger) (sweepResolution, error) {
	repo, err := ParseRepo(p.Repo)
	if err != nil {
		return sweepResolution{}, err
	}
	expired := r.c.now().Sub(p.At) > r.c.cfg.ResolverHorizon

	runs, err := r.candidateRuns(ctx, repo, p, p.At.Add(-r.c.cfg.ResolverLookback))
	if err != nil {
		// The horizon must not expire a trigger just because GitHub was
		// unreachable, so an error defers rather than resolving to `none`.
		return sweepResolution{}, err
	}

	// TIER 2: the injected standby_trigger_id in the run-name.
	//
	// Tier 1 never reaches here — a trigger whose dispatch response carried a
	// run id already has an active `exact` run_link, and the pending-correlation
	// query's NOT EXISTS drops it. So the resolver only ever sees triggers where the
	// response was lost, tier 1 was off, or the link is still weak.
	//
	// The gate is ArmsRunNameMatch, i.e. correlation == "injected" exactly.
	// "dispatch_response" must NOT enter this branch: nothing was injected, so
	// the search cannot match, and a non-match here defers the trigger instead
	// of letting tier 3 resolve it. The field the name is read from is the
	// run-name seam, measured correct on workflow_dispatch on 2026-08-21 —
	// see runname.go.
	tier2 := tier2NotArmed
	if p.Correlation.ArmsRunNameMatch() {
		for _, run := range runs {
			if RunCarriesTriggerID(run, p.EventID) {
				return sweepResolution{
					link: RunLink{
						TriggerEventID:   p.EventID,
						RunID:            run.GetID(),
						Resolution:       ResolutionExact,
						ResolutionSource: ResolutionSourceRunName,
						At:               r.c.now(),
					},
					run:   run,
					write: true,
					tier2: tier2Matched,
				}, nil
			}
		}
		if !expired {
			// Still inside the horizon, so "the run does not exist yet" is
			// live and no claim about the run-name can be made. This is the
			// boundary of what a single lookup can decide.
			return sweepResolution{tier2: tier2Pending}, nil
		}
		// Past the horizon with no exact match. The injection was supposed to
		// make this certain, so falling through to the time window is a
		// weaker answer — but `inferred` and `ambiguous` say so in the record,
		// and a bare `none` would throw away a usable link.
		//
		// The window match below now also carries diagnostic weight: if it
		// finds a run for this trigger, then a run existed and did not carry
		// the id we injected, which is the measured "run-name not applied"
		// case (runname.go). Classified after the match, since that is where
		// the candidate count becomes known.
		tier2 = tier2NoCandidate
	}

	// TIER 3: time window plus ref. Every link written from here
	// carries resolution_source "window", including `none`.
	best, count := r.matchWindow(runs, p)
	if tier2 == tier2NoCandidate && count > 0 {
		tier2 = tier2Absent
	}
	switch {
	case count == 1:
		return sweepResolution{
			link: RunLink{
				TriggerEventID:   p.EventID,
				RunID:            best.GetID(),
				Resolution:       ResolutionInferred,
				ResolutionSource: ResolutionSourceWindow,
				At:               r.c.now(),
			},
			run:   best,
			write: true,
			tier2: tier2,
		}, nil
	case count > 1:
		return sweepResolution{
			link: RunLink{
				TriggerEventID:   p.EventID,
				RunID:            best.GetID(),
				Resolution:       ResolutionAmbiguous,
				ResolutionSource: ResolutionSourceWindow,
				At:               r.c.now(),
			},
			run:   best,
			write: true,
			tier2: tier2,
		}, nil
	case expired:
		// The horizon elapsed with no candidate. This is written
		// explicitly, not left NULL — an unresolvable trigger is a fact the
		// inventory has to be able to count.
		return sweepResolution{
			link: RunLink{
				TriggerEventID:   p.EventID,
				Resolution:       ResolutionNone,
				ResolutionSource: ResolutionSourceWindow,
				At:               r.c.now(),
			},
			write: true,
			tier2: tier2,
		}, nil
	default:
		return sweepResolution{tier2: tier2}, nil
	}
}

// candidateRuns lists the runs that could belong to a pending trigger, created
// at or after `since`.
//
// Scoped to the workflow, not the repo, and paged at 100 (a
// repo-wide query with a small page size loses the candidate on a busy
// repo).
//
// `since` is a parameter and not derived from p.At because ResolveOne
// anchors the listing on the ORIGINAL attempt's window, which is not p.At.
func (r *Resolver) candidateRuns(ctx context.Context, repo Repo, p PendingTrigger, since time.Time) ([]*github.WorkflowRun, error) {
	workflowID, err := r.workflowID(ctx, repo, p.Workflow)
	if err != nil {
		return nil, err
	}

	opts := &github.ListWorkflowRunsOptions{
		Event:       TriggerWorkflowDispatch,
		Created:     ">=" + since.UTC().Format(time.RFC3339),
		ListOptions: github.ListOptions{PerPage: perPage},
	}

	page, _, err := call(ctx, r.c, repo, LaneDispatch,
		func(ctx context.Context, gh *github.Client) (*github.WorkflowRuns, *github.Response, error) {
			return gh.Actions.ListWorkflowRunsByID(ctx, repo.Owner, repo.Name, workflowID, opts)
		})
	if err != nil {
		return nil, err
	}
	// One page only. The window is minutes wide and scoped to one workflow;
	// paging further would spend quota on runs that are older than the
	// window by construction.
	return page.WorkflowRuns, nil
}

func (r *Resolver) workflowID(ctx context.Context, repo Repo, workflow string) (int64, error) {
	entry, ok, err := r.c.store.GetWorkflowCache(ctx, repo.String(), workflow, WorkflowCacheDefaultRef)
	if err == nil && ok && entry.WorkflowID != 0 {
		return entry.WorkflowID, nil
	}
	return r.c.resolveWorkflowID(ctx, repo, workflow)
}

// matchWindow returns the candidate closest in time to the dispatch, and how
// many candidates there were. Zero candidates returns (nil, 0).
func (r *Resolver) matchWindow(runs []*github.WorkflowRun, p PendingTrigger) (*github.WorkflowRun, int) {
	return matchWindowBounds(runs, p.Ref, p.At,
		p.At.Add(-r.c.cfg.ResolverLookback), p.At.Add(r.c.cfg.ResolverHorizon))
}

// matchWindowBounds is matchWindow with the window stated explicitly. It is
// shared with ResolveOne, whose window is the ORIGINAL attempt's and
// therefore cannot be derived from the trigger's own timestamp.
//
// `at` is the instant proximity is measured from; it only orders the
// candidates and never widens or narrows the window.
func matchWindowBounds(runs []*github.WorkflowRun, ref string, at, lower, upper time.Time) (*github.WorkflowRun, int) {
	var (
		best  *github.WorkflowRun
		count int
		delta time.Duration
	)
	for _, run := range runs {
		created := run.GetCreatedAt().Time
		if created.Before(lower) || created.After(upper) {
			continue
		}
		if !RefMatchesHeadBranch(ref, run.GetHeadBranch()) {
			continue
		}
		count++
		d := created.Sub(at)
		if d < 0 {
			d = -d
		}
		if best == nil || d < delta {
			best, delta = run, d
		}
	}
	return best, count
}
