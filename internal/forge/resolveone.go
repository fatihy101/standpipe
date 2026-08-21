package forge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v90/github"
)

// ---------------------------------------------------------------------------
// The pre-retry resolver check.
//
// After a timeout or a 5xx the idempotency row stays `unknown` and the same
// key MAY be retried. Before that retry dispatches, the run-link resolver is
// run against the ORIGINAL time window. If a
// matching run exists, the record is completed as `forwarded` and THE DISPATCH
// IS NOT REPEATED.
//
// Resolver.Sweep cannot serve this. It is batch, it is driven by
// CorrelationStore.PendingCorrelations, and that query selects triggers whose
// outcome is `forwarded`. A reclaimed trigger's prior outcome is `error`, so
// the sweep never sees it — the retry would dispatch a second time into a
// workflow that may already be running. That is the exact failure this product
// exists to prevent (a dispatch timeout is ambiguous, so it is never retried
// automatically).
//
// ResolveOne is the single-trigger entry point. It is READ-ONLY: it calls
// GitHub, it does not write to the store and it does not emit a counter. The
// caller owns the write — completing the idempotency row and appending the
// trigger_outcome — and that write must stay in the caller's transaction, so
// that no state exists in which the run was found but the link was lost.
// ---------------------------------------------------------------------------

// ErrResolveInconclusive is returned whenever ResolveOne could not determine
// whether a run exists.
//
// It exists so that `err == nil` is exactly equivalent to "the verdict is
// definitive". A caller that only checks the error, and ignores the outcome,
// therefore cannot mistake "I could not tell" for "there is no run".
var ErrResolveInconclusive = errors.New("forge: pre-retry resolve was inconclusive")

// ResolveVerdict is ResolveOne's answer to "did this trigger already produce a
// run?".
//
// THE ZERO VALUE IS VerdictUnknown, deliberately. A ResolveOutcome that was
// never filled in, or was filled in by a call that failed, reads as "I do not
// know" and never as "no run" — the value that would authorise a second
// dispatch.
type ResolveVerdict string

const (
	// VerdictUnknown: no answer. The caller MUST NOT dispatch on the strength
	// of this value and MUST NOT complete the record as forwarded either.
	// Neither branch is supported by evidence. The honest handling is to fail
	// the retry (503 + Retry-After) and let the caller come back, which leaves
	// the idempotency row `unknown` — the state that is already correct.
	VerdictUnknown ResolveVerdict = ""
	// VerdictRunFound: a run attributable to the original attempt exists
	// upstream. Complete the record as forwarded, write Link,
	// and DO NOT DISPATCH.
	VerdictRunFound ResolveVerdict = "run_found"
	// VerdictNoRun: the original attempt produced no run. The retry may
	// dispatch. Read ResolveOne's blind spots below before trusting this:
	// it is a well-evidenced negative, not a proof.
	VerdictNoRun ResolveVerdict = "no_run"
)

// RunAlreadyExists reports whether the dispatch must be suppressed.
func (v ResolveVerdict) RunAlreadyExists() bool { return v == VerdictRunFound }

// SafeToDispatch reports whether the retry may go to GitHub.
//
// It is deliberately NOT the negation of RunAlreadyExists: both are false for
// VerdictUnknown, so a two-branch if/else at the call site cannot silently
// route the unknown case into either action. The third branch has to be
// written out.
func (v ResolveVerdict) SafeToDispatch() bool { return v == VerdictNoRun }

// ResolveReason says how the verdict was reached. It is for the log line and
// the operator; nothing branches on it.
type ResolveReason string

const (
	// ReasonRunNameMatch: tier 2. The injected standby_trigger_id was found in
	// a run name. This is proof, not inference.
	ReasonRunNameMatch ResolveReason = "run_name_match"
	// ReasonWindowMatch: tier 3, exactly one candidate in the original window
	// on a matching ref.
	ReasonWindowMatch ResolveReason = "window_match"
	// ReasonWindowMatchAmbiguous: tier 3 with more than one candidate. The
	// nearest was taken and Link.Resolution says `ambiguous`.
	ReasonWindowMatchAmbiguous ResolveReason = "window_match_ambiguous"
	// ReasonNeverDispatched: the superseded attempt never reached the dispatch
	// stage, so nothing left this process for GitHub to have run.
	ReasonNeverDispatched ResolveReason = "never_dispatched"
	// ReasonNoCandidate: the window was searched and held no matching run.
	ReasonNoCandidate ResolveReason = "no_candidate"
	// ReasonCandidatesTruncated: the candidate page came back full, so the
	// target run may be on a page that was not read. A negative cannot be
	// claimed from a truncated listing.
	ReasonCandidatesTruncated ResolveReason = "candidates_truncated"
	// ReasonUpstreamError: GitHub could not be asked.
	ReasonUpstreamError ResolveReason = "upstream_error"
)

// Tier2Status records what happened to the exact (run-name) search. It is the
// difference between a proof and an inference, so it is reported rather than
// left implicit.
type Tier2Status string

const (
	// Tier2NotArmed: the original dispatch injected nothing, so no run name
	// can carry a trigger id. Correlation was not "injected".
	Tier2NotArmed Tier2Status = "not_armed"
	// Tier2Searched: the run names were searched for ResolveWindow.OriginalEventID.
	Tier2Searched Tier2Status = "searched"
	// Tier2OriginalIDUnavailable: tier 2 WAS armed, but the caller could not
	// supply the id that was injected — see ResolveWindow.OriginalEventID.
	// The exact search was skipped rather than run against the wrong id, and
	// the answer degrades to tier 3. THIS IS A DEGRADATION AND IT IS REPORTED
	// (no degradation may be silent); it is not an error.
	Tier2OriginalIDUnavailable Tier2Status = "original_event_id_unavailable"
	// Tier2SearchedRunNameAbsent: the run names WERE searched, no run carried
	// the injected id, AND the window nevertheless held a run this trigger
	// could own. It refines Tier2Searched — the search ran either way — and
	// exists because that combination is evidence about the run-name rather
	// than about timing: a plausible run was there and it did not carry what
	// we injected.
	//
	// Measured on 2026-08-21 to be a real GitHub behaviour and not only an
	// operator mistake: a declared `run-name:` is sometimes not applied to
	// the run at all (runname.go). It is NOT proof — the candidate may be
	// somebody else's dispatch, and the operator's template may simply not
	// interpolate standby_trigger_id. Nothing branches on it; it is for the
	// log line, exactly like Reason.
	//
	// With zero candidates the status stays Tier2Searched, because "nothing
	// was there to carry a name" says nothing about names.
	Tier2SearchedRunNameAbsent Tier2Status = "searched_run_name_absent"
)

// ResolveWindow is the ORIGINAL attempt's dispatch window, from
// store.ReserveResult on a ReserveReclaimed outcome.
//
// The pre-retry rule is explicit that the resolver runs against the original window and not
// against now. The retry may arrive minutes after the attempt it supersedes; a
// window anchored on the retry would look at the wrong stretch of time and
// return a confident, wrong negative.
type ResolveWindow struct {
	// AttemptAt is ReserveResult.OriginalAttemptAt: when the superseded
	// attempt took the key. Required.
	AttemptAt time.Time
	// DispatchAt is ReserveResult.OriginalDispatchAt: when that attempt
	// stamped dispatch_started_at, which store.MarkDispatchStarted commits
	// BEFORE the POST goes out.
	//
	// nil therefore means the attempt never reached GitHub, which is a
	// definitive negative for the time-window tier: any run found in the
	// window would belong to somebody else, and suppressing the dispatch on it
	// would silently swallow a trigger that was never sent. See the blind-spot
	// note on ResolveOne for the case where this is nil and still wrong.
	DispatchAt *time.Time
	// OriginalEventID is the trigger event id the SUPERSEDED attempt carried —
	// the value that was injected as standby_trigger_id and that a run name
	// would carry. It is NOT PendingTrigger.EventID: the reclaim CAS replaces
	// idempotency.event_id with this attempt's new event, so the new id was
	// never injected anywhere and searching for it can only miss.
	//
	// Empty disables the exact tier and is reported as
	// Tier2OriginalIDUnavailable. Do not substitute the new event id to fill
	// the field — that turns a reported degradation into a search that is
	// guaranteed to fail for a reason nothing records.
	OriginalEventID string
}

// anchor is the instant the window is measured from: the moment we actually
// posted, when we know it, and otherwise the moment the attempt began.
func (w ResolveWindow) anchor() time.Time {
	if w.DispatchAt != nil && !w.DispatchAt.IsZero() {
		return *w.DispatchAt
	}
	return w.AttemptAt
}

// ResolveOutcome is ResolveOne's full answer. Only Verdict decides anything;
// the rest exists so the decision is explicable after the fact.
type ResolveOutcome struct {
	Verdict ResolveVerdict
	Reason  ResolveReason
	// Link is the run_link the caller should write, valid only when Verdict is
	// VerdictRunFound. It is built here so no call site spells a
	// resolution_source literal: a hand-spelled literal that drifts would
	// misattribute the tier with no symptom.
	//
	// TriggerEventID is set to the trigger passed in — the RETRY's event, the
	// one completed as `forwarded`. The link is deliberately not attached
	// to the superseded event, whose outcome is `error` and which no correlation
	// query selects.
	Link RunLink
	// Run is the matched workflow run, nil unless Verdict is VerdictRunFound.
	// Its conclusion, when it has one, is a run_observed the caller may
	// record.
	Run *github.WorkflowRun
	// Tier2 says whether the exact search ran, and if not, why.
	Tier2 Tier2Status
	// WindowLower and WindowUpper are the bounds actually searched. Logged so
	// a wrong answer can be attributed to the window rather than to the match.
	WindowLower time.Time
	WindowUpper time.Time
	// CandidatesListed is how many runs the listing returned.
	CandidatesListed int
}

// ResolveOne answers "did this trigger already produce a run?" for a single
// trigger, against the original attempt's window.
//
// It is the pre-dispatch guard on the ReserveReclaimed path.
// Resolver.Sweep will never cover that path: its query selects `forwarded`
// triggers and a reclaimed trigger's prior outcome is `error`.
//
// p supplies the target (Repo, Workflow, Ref), the armament
// (p.Correlation, which must be mapped from the stored trigger row: left
// zero it silently reads as "tier 2 not armed", exactly as in the sweep) and p.EventID, which is the RETRY's event id and is used only to
// address the returned RunLink. p.At is not read: the window comes from w.
//
// Contract:
//
//   - err == nil ⟺ the verdict is definitive (VerdictRunFound or VerdictNoRun).
//   - err != nil ⟹ Verdict is VerdictUnknown. It is NOT permission to
//     dispatch, and it is NOT grounds to complete the record as forwarded.
//   - Nothing is written. Not to the store, not to the metrics registry.
//
// BLIND SPOTS — what a VerdictNoRun cannot rule out:
//
//  1. THE RUN ROW MAY NOT EXIST YET — MEASURED, NOT SUPPOSED. GitHub creates
//     the workflow_run row asynchronously after accepting the dispatch. On
//     2026-08-21 a listing made immediately after a parameterless 204 found
//     NOTHING and one about two seconds later found the run. The gap is a BOUND, not a number: greater than zero, under
//     roughly two seconds, on one repo with an idle queue — do not tune any
//     ticker to it. A retry that arrives seconds after the original timeout can
//     therefore search a window in which the run is real but not yet listable,
//     and without tier 2 nothing here distinguishes that from a dispatch that
//     never landed. VerdictNoRun is consequently not evidence the dispatch
//     failed: it is neither grounds to re-dispatch (the no-automatic-retry rule is
//     absolute)
//     nor grounds to write `resolution: none`, which only ResolverHorizon
//     writes and which sits orders of magnitude above this gap. With tier 1
//     armed the gap does not arise at all — return_run_details was measured to
//     return only once the row exists.
//  2. TIER 3 IS INFERENCE. With Tier2 != Tier2Searched the only evidence is
//     "a workflow_dispatch run of this workflow on this ref, inside the
//     window". A concurrent dispatch by anybody else satisfies it, and our own
//     run failing to appear does not violate it.
//  3. DispatchAt == nil IS TRUSTED. It reads as "nothing was posted", which
//     holds because MarkDispatchStarted commits before the POST. It stops
//     holding across a CHAIN of reclaims: the reclaim CAS sets
//     dispatch_started_at = NULL, so if attempt 1 dispatched and attempt 2
//     died before its own stamp, attempt 3 is told nothing was ever
//     dispatched. Tier 2 still catches that case; tier 3 does not.
//  4. THE LISTING IS ONE PAGE. A full page yields VerdictUnknown rather than a
//     negative, so this is reported and not silent — but it means a busy repo
//     converts to "inconclusive" rather than to an answer.
//  5. IT ANSWERS ABOUT A RUN, NOT ABOUT A DISPATCH. A dispatch GitHub accepted
//     and then dropped, or one whose run was deleted, is indistinguishable
//     from one that never arrived.
func (r *Resolver) ResolveOne(ctx context.Context, p PendingTrigger, w ResolveWindow) (ResolveOutcome, error) {
	out := ResolveOutcome{Verdict: VerdictUnknown, Reason: ReasonUpstreamError}

	anchor := w.anchor()
	if anchor.IsZero() {
		return out, fmt.Errorf("%w: the original attempt window is empty (AttemptAt and DispatchAt are both zero)",
			ErrResolveInconclusive)
	}
	out.WindowLower = anchor.Add(-r.c.cfg.ResolverLookback)
	out.WindowUpper = anchor.Add(r.c.cfg.ResolverHorizon)

	switch {
	case !p.Correlation.ArmsRunNameMatch():
		out.Tier2 = Tier2NotArmed
	case w.OriginalEventID == "":
		out.Tier2 = Tier2OriginalIDUnavailable
	default:
		out.Tier2 = Tier2Searched
	}

	// Nothing was posted and the exact search is unavailable, so there is
	// nothing an API call could establish: a window match would necessarily be
	// somebody else's run. Answering from what we already know beats spending
	// a call to acquire evidence we would have to discard.
	if w.DispatchAt == nil && out.Tier2 != Tier2Searched {
		out.Verdict, out.Reason = VerdictNoRun, ReasonNeverDispatched
		return out, nil
	}

	repo, err := ParseRepo(p.Repo)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrResolveInconclusive, err)
	}

	runs, err := r.candidateRuns(ctx, repo, p, out.WindowLower)
	if err != nil {
		// This is a guard, so an unreachable GitHub is inconclusive and never
		// a negative. The caller decides what to do with a retry it cannot
		// clear; it must not be this function's silence.
		return out, fmt.Errorf("%w: %v", ErrResolveInconclusive, err)
	}
	out.CandidatesListed = len(runs)

	// TIER 2 — proof. The id was injected into the dispatch that timed out, so
	// a run carrying it in its name IS that dispatch's run. This is the only
	// tier that survives a lost response, and the only one that answers
	// correctly when the reclaim chain has erased the dispatch stamp.
	if out.Tier2 == Tier2Searched {
		for _, run := range runs {
			if RunCarriesTriggerID(run, w.OriginalEventID) {
				out.Verdict, out.Reason, out.Run = VerdictRunFound, ReasonRunNameMatch, run
				out.Link = RunLink{
					TriggerEventID:   p.EventID,
					RunID:            run.GetID(),
					Resolution:       ResolutionExact,
					ResolutionSource: ResolutionSourceRunName,
					At:               r.c.now(),
				}
				return out, nil
			}
		}
	}

	// The exact search found nothing and nothing was ever posted. Tier 3 must
	// not run: with no dispatch of ours in this window, every candidate in it
	// belongs to someone else, and treating one as ours would suppress a
	// trigger that never left the building.
	if w.DispatchAt == nil {
		out.Verdict, out.Reason = VerdictNoRun, ReasonNeverDispatched
		return out, nil
	}

	// TIER 3 — inference, on the original window. A match here suppresses the
	// dispatch: an ambiguous timeout makes a duplicate production run the failure to avoid,
	// and Link.Resolution records that the answer was inferred rather than
	// proven.
	best, count := matchWindowBounds(runs, p.Ref, anchor, out.WindowLower, out.WindowUpper)
	if out.Tier2 == Tier2Searched && count > 0 {
		// The exact search ran, found nothing, and the window is not empty.
		// Report it: the verdict below is a tier-3 inference on a trigger
		// that was supposed to be provable, and that difference is invisible
		// from Verdict and Reason alone.
		out.Tier2 = Tier2SearchedRunNameAbsent
	}
	switch {
	case count == 1:
		out.Verdict, out.Reason, out.Run = VerdictRunFound, ReasonWindowMatch, best
		out.Link = RunLink{
			TriggerEventID:   p.EventID,
			RunID:            best.GetID(),
			Resolution:       ResolutionInferred,
			ResolutionSource: ResolutionSourceWindow,
			At:               r.c.now(),
		}
		return out, nil
	case count > 1:
		out.Verdict, out.Reason, out.Run = VerdictRunFound, ReasonWindowMatchAmbiguous, best
		out.Link = RunLink{
			TriggerEventID:   p.EventID,
			RunID:            best.GetID(),
			Resolution:       ResolutionAmbiguous,
			ResolutionSource: ResolutionSourceWindow,
			At:               r.c.now(),
		}
		return out, nil
	}

	// A full page means the listing was cut, not that the repo was quiet.
	// Claiming a negative from it is how a duplicate dispatch gets authorised
	// by an answer that was never actually computed.
	if len(runs) >= perPage {
		out.Verdict, out.Reason = VerdictUnknown, ReasonCandidatesTruncated
		return out, fmt.Errorf("%w: the candidate listing filled its page (%d runs since %s); the run may be on a page that was not read",
			ErrResolveInconclusive, len(runs), out.WindowLower.UTC().Format(time.RFC3339))
	}

	out.Verdict, out.Reason = VerdictNoRun, ReasonNoCandidate
	return out, nil
}
