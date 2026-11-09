package forge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/go-github/v90/github"
)

// ReservedInputKey is the input the gateway injects for correlation (tier 2).
// A caller may not send it: rule 3 makes that a 400 bad_request with detail
// reserved_input_key — the gateway's own decision, distinct from the unsupported-input
// rule's 422,
// which reports an upstream-shaped rejection.
const ReservedInputKey = "standby_trigger_id"

// MaxDispatchInputs is GitHub's workflow_dispatch input ceiling and the gate
// tier-2 injection respects (injection rule 2; raised from 10 to 25).
//
// The gate now sits exactly on GitHub's limit rather than below it: 24 caller
// inputs still get an injection and go out as 25. If 25 is wrong, the result
// is GitHub's 422, surfaced as upstream_rejected, not a silent miscount. The
// 25 was read from the go-github v90 source, not measured by a real call.
const MaxDispatchInputs = 25

// Correlation records which tier of the correlation hierarchy a dispatch
// was ARMED with.
//
// It is decided during request processing, before the dispatch leaves, and lives
// inside the content_sha256 hash scope on a `trigger` row that is never
// updated. It therefore cannot record which tier finally produced the link;
// that fact is written after the response, on run_link.resolution_source
// (see ResolutionSource).
type Correlation string

const (
	// CorrelationInjected: standby_trigger_id went out with the dispatch, so
	// tier 2 is armed and an exact run-name match is possible even if the
	// dispatch response is lost. Tier 1 is normally armed as well; this value
	// still wins — see correlationFor.
	CorrelationInjected Correlation = "injected"
	// CorrelationDispatchResponse: tier 1 only. return_run_details was
	// requested but injection could not happen (injection rule 1 or 2). The link is
	// exact while the response arrives, and falls straight to tier 3 when it
	// does not.
	CorrelationDispatchResponse Correlation = "dispatch_response"
	// CorrelationWindow: neither tier 1 nor tier 2. Reachable only when
	// github.return_run_details is false AND injection could not happen, so it
	// does not occur in a default deployment. It exists so
	// the escape hatch can record what actually happened.
	CorrelationWindow Correlation = "window"
)

// correlationFor decides the armament label. It is the ONLY place a
// Correlation value is chosen, so the three branches cannot drift apart.
//
// THE ORDERING IS BY DURABILITY, NOT BY STRENGTH. Tiers 1 and 2 both produce
// resolution "exact", so "strongest tier wins" has no meaning here. What
// separates them is that tier 2 survives a lost dispatch response: the run
// name carries the trigger id on GitHub's side whether or not we ever read the
// reply. So an injected dispatch is labelled "injected" even though tier 1 was
// also armed and will almost always answer first.
//
// A strength-ordered variant would label every dispatch "dispatch_response"
// (tier 1 is armed on every dispatch), making "injected" unreachable and
// deleting the operator-checklist hardening signal — "is this repo ready for an outage?" —
// with no test failure and no external symptom. Do not "simplify" this.
func correlationFor(injected, tier1Armed bool) Correlation {
	switch {
	case injected:
		return CorrelationInjected
	case tier1Armed:
		return CorrelationDispatchResponse
	default:
		return CorrelationWindow
	}
}

// ArmsRunNameMatch reports whether tier 2 (the standby_trigger_id run-name
// search) is available for a trigger carrying this correlation.
//
// Only "injected" arms it. Do NOT rewrite this as a negative test — anything
// of the shape `c != CorrelationWindow` silently promotes tier-1-only
// triggers into the tier-2 branch, where the resolver searches run names for
// an id that was never injected. That search cannot match, so the trigger is
// held pending until the horizon expires instead of being resolved from the
// time window, and the log reads "tier 2 tried and failed" when tier 2 was
// never armed at all.
func (c Correlation) ArmsRunNameMatch() bool { return c == CorrelationInjected }

// InjectionSkipReason says why tier-2 injection was skipped. Empty when it was
// not skipped.
//
// It is not the same fact as Correlation and is not written to the event: a
// skipped injection may still be armed with tier 1. It exists so the reason is
// available to the log line and the operator: no degradation may be silent.
type InjectionSkipReason string

const (
	// SkipInputNotDeclared: the workflow's inputs_def has no
	// standby_trigger_id. Injecting anyway would send an undefined input,
	// GitHub would answer 422, and the dispatch would die entirely — which is
	// exactly why injection is conditional (injection rule 1).
	SkipInputNotDeclared InjectionSkipReason = "trigger_input_not_declared"
	// SkipInputBudgetFull: the caller already filled the input budget
	// (injection rule 2).
	SkipInputBudgetFull InjectionSkipReason = "input_budget_full"
	// SkipCacheCold: the workflow definition has never been fetched, so
	// rule 1 cannot be evaluated. Skipping is the fail-safe direction.
	SkipCacheCold InjectionSkipReason = "workflow_cache_cold"
)

// InputsValidation is the closed enum recorded on every trigger event.
// It carries the reason, not a boolean, so a skipped validation is visible.
type InputsValidation string

const (
	InputsValidationDefaultBranch        InputsValidation = "default_branch"
	InputsValidationExactRef             InputsValidation = "exact_ref"
	InputsValidationSkippedColdCache     InputsValidation = "skipped_cold_cache"
	InputsValidationSkippedNonDefaultRef InputsValidation = "skipped_non_default_ref"
)

// DispatchRequest is one canonical trigger resolved to a target.
type DispatchRequest struct {
	Repo     Repo
	Workflow string // workflow file name, e.g. "release.yml"
	// Ref is the canonical full ref from the request body. Request
	// validation rejects a short name before this point.
	Ref string
	// Inputs are the caller's inputs. Values are strings: GitHub serializes
	// workflow_dispatch inputs to strings regardless of declared type.
	Inputs map[string]string
	// TriggerEventID is the trigger event's ULID/UUIDv7. It is what gets
	// injected and what the resolver searches the run-name for.
	TriggerEventID string
}

// PlanOptions carries the deployment-level facts PlanDispatch needs that are
// not properties of the request. It is a struct, not another positional bool,
// because the value it carries decides a correlation label that has no
// external symptom when it is wrong.
type PlanOptions struct {
	// ReturnRunDetails mirrors github.return_run_details: whether tier 1
	// is armed on this deployment. It is the ONLY thing that separates
	// correlation "dispatch_response" from "window".
	ReturnRunDetails bool
}

// DispatchPlan is the decision taken before the call goes out. It is produced
// separately from the call so the trigger event can be written with the right
// correlation and validation markers in the same transaction as the
// reservation, before anything leaves for GitHub.
type DispatchPlan struct {
	WorkflowID          int64
	Ref                 string
	Inputs              map[string]string
	Correlation         Correlation
	InjectionSkipReason InjectionSkipReason
	InputsValidation    InputsValidation
	// ReturnRunDetails is what the plan will ask GitHub for. Carried on
	// the plan rather than read from config at call time so that the dispatch
	// and the recorded correlation cannot disagree.
	ReturnRunDetails bool
	// UnsupportedKeys is non-empty when the caller sent input keys the
	// workflow does not declare. The unsupported-input rule: the request is
	// rejected with 422 and never forwarded.
	UnsupportedKeys []string
}

// Rejected reports whether the plan must not be dispatched (unsupported inputs).
func (p DispatchPlan) Rejected() bool { return len(p.UnsupportedKeys) > 0 }

// DispatchResult is the outcome of a call that reached GitHub.
type DispatchResult struct {
	Plan             DispatchPlan
	UpstreamDuration time.Duration

	// UpstreamStatus is the status GitHub actually returned, recorded verbatim
	// into trigger_outcome.outcome_http_status.
	//
	// IT IS 200 WHEN TIER 1 IS ARMED AND 204 WHEN IT IS NOT. Both mean
	// "forwarded". The value is therefore NOT comparable across the
	// return_run_details cutover, and no query may treat 204 as the definition
	// of success: the authoritative signal is outcome_status = 'forwarded'
	// (the correlation resolver and the coverage-gap report both already
	// filter on it). ReturnRunDetailsRequested below
	// says which regime produced the number, so the mixed column stays
	// readable instead of being normalised into a lie.
	UpstreamStatus int
	// ReturnRunDetailsRequested is whether this call asked for run details. It
	// is the interpretation key for UpstreamStatus, and it distinguishes "tier
	// 1 was off" from "tier 1 was on and the run id did not come back".
	ReturnRunDetailsRequested bool

	// RunID is the run GitHub reported in the dispatch response (correlation tier 1).
	// Zero when tier 1 is disabled, when GitHub returned no body, or when the
	// response was lost — the caller must treat zero as "not linked yet", never
	// as a failure.
	RunID int64
	// RunURL is the API URL from the same body (`run_url`).
	RunURL string
	// HTMLURL is the browser URL from the same body (`html_url`).
	//
	// The gateway's trigger response field is named `run_url` but its
	// documented example value is the github.com URL, i.e. GitHub's html_url,
	// while GitHub's own dispatch response keeps the two distinct. Both are
	// surfaced here so the composition root can pick without this package
	// guessing.
	HTMLURL string
}

// PlanDispatch decides the outgoing inputs and the correlation marker without
// calling GitHub.
//
// The four tier-2 injection rules, in order:
//  1. inject only when standby_trigger_id is declared in the cached inputs_def;
//  2. skip when the caller already sent MaxDispatchInputs inputs;
//  3. a caller-supplied standby_trigger_id is rejected outright;
//  4. when injection is skipped, the event is marked with the tier it still
//     has — "dispatch_response" if tier 1 is armed, "window" if it is not.
func PlanDispatch(req DispatchRequest, entry WorkflowCacheEntry, cached bool, defaultBranch string, opts PlanOptions) (DispatchPlan, error) {
	// Rule 3 first: a caller-supplied reserved key is refused before anything
	// else, so it can never reach GitHub or be confused with the plan.
	if _, ok := req.Inputs[ReservedInputKey]; ok {
		return DispatchPlan{}, &Error{
			Kind:    KindReservedInputKey,
			Message: fmt.Sprintf("%q is reserved by the gateway", ReservedInputKey),
		}
	}

	plan := DispatchPlan{
		Ref:                 DispatchRef(req.Ref),
		Inputs:              make(map[string]string, len(req.Inputs)+1),
		Correlation:         correlationFor(false, opts.ReturnRunDetails),
		InjectionSkipReason: SkipCacheCold,
		ReturnRunDetails:    opts.ReturnRunDetails,
	}
	for k, v := range req.Inputs {
		plan.Inputs[k] = v
	}
	if cached {
		plan.WorkflowID = entry.WorkflowID
	}

	switch {
	case !cached || entry.InputsDef == nil:
		// The definition has never been fetched. Validation is skipped,
		// the request is still forwarded, and the event says so.
		plan.InputsValidation = InputsValidationSkippedColdCache
		plan.InjectionSkipReason = SkipCacheCold
		return plan, nil

	case defaultBranch != "" && ShortRef(req.Ref) != defaultBranch:
		// Phase 0 simplification: the cache is filled from the default
		// branch only, so a non-default ref may declare different inputs.
		// Validating against the wrong branch would reject valid requests.
		plan.InputsValidation = InputsValidationSkippedNonDefaultRef

	default:
		plan.InputsValidation = InputsValidationDefaultBranch
		for key := range req.Inputs {
			if _, ok := entry.InputsDef[key]; !ok {
				plan.UnsupportedKeys = append(plan.UnsupportedKeys, key)
			}
		}
		if len(plan.UnsupportedKeys) > 0 {
			sortStrings(plan.UnsupportedKeys)
			return plan, nil
		}
	}

	// Rule 1.
	if _, declared := entry.InputsDef[ReservedInputKey]; !declared {
		plan.InjectionSkipReason = SkipInputNotDeclared
		return plan, nil
	}
	// Rule 2. The caller's own inputs already fill the budget, so adding one
	// more would exceed it.
	if len(req.Inputs) >= MaxDispatchInputs {
		plan.InjectionSkipReason = SkipInputBudgetFull
		return plan, nil
	}
	// Rule 4's inverse: conditions met, inject. Tier 2 is now armed, and that
	// is what the event records regardless of tier 1 — see correlationFor.
	plan.Inputs[ReservedInputKey] = req.TriggerEventID
	plan.Correlation = correlationFor(true, opts.ReturnRunDetails)
	plan.InjectionSkipReason = ""
	return plan, nil
}

// Plan builds a DispatchPlan from the persisted workflow cache.
func (c *Client) Plan(ctx context.Context, req DispatchRequest) (DispatchPlan, error) {
	if c.store == nil {
		return DispatchPlan{}, errors.New("forge: no store configured")
	}
	entry, ok, err := c.store.GetWorkflowCache(ctx, req.Repo.String(), req.Workflow, WorkflowCacheDefaultRef)
	if err != nil {
		return DispatchPlan{}, err
	}
	if !ok {
		// A cold cache is a degradation mode and gets a counter. It is not an
		// error — the request still goes through, unvalidated and marked.
		c.metrics.IncCounter("gateway_workflow_cache_cold_total",
			map[string]string{"repo": req.Repo.String(), "workflow": req.Workflow})
	}
	return PlanDispatch(req, entry, ok, entry.DefaultBranch,
		PlanOptions{ReturnRunDetails: c.cfg.tier1Armed()})
}

// Dispatch forwards a planned trigger to GitHub.
//
// It NEVER retries. A timeout on POST .../dispatches is ambiguous — the
// dispatch may already have landed — so the call fails with
// KindUpstreamTimeout, the trigger stays `unknown`, and the correlation resolver
// settles it. A retry here would double-trigger a production workflow.
func (c *Client) Dispatch(ctx context.Context, req DispatchRequest, plan DispatchPlan) (*DispatchResult, error) {
	if plan.Rejected() {
		return nil, &Error{
			Kind:    KindUpstreamRejected,
			Message: fmt.Sprintf("workflow does not declare inputs: %v", plan.UnsupportedKeys),
		}
	}

	// gateway_correlation_total{correlation} (docs/reference/metrics.md). Emitted here rather than
	// in Plan because a rejected plan never becomes a trigger row, and
	// counting armament for a request that was never dispatched would inflate
	// the operator-checklist hardening ratio. Every plan that reaches this point has a
	// `trigger` row carrying this exact value: it was written before the dispatch.
	c.metrics.IncCounter("gateway_correlation_total",
		map[string]string{"correlation": string(plan.Correlation)})

	workflowID := plan.WorkflowID
	if workflowID == 0 {
		// The workflow ID is resolved on first use and cached.
		id, err := c.resolveWorkflowID(ctx, req.Repo, req.Workflow)
		if err != nil {
			return nil, err
		}
		workflowID = id
	}

	body := github.CreateWorkflowDispatchEventRequest{
		Ref:    plan.Ref,
		Inputs: toAnyMap(plan.Inputs),
	}
	if plan.ReturnRunDetails {
		enabled := true
		body.ReturnRunDetails = &enabled
	}

	started := time.Now()
	details, resp, err := callWithTimeout(ctx, c, req.Repo, LaneDispatch, dispatchTimeout(c.cfg),
		func(ctx context.Context, gh *github.Client) (*github.WorkflowDispatchRunDetails, *github.Response, error) {
			return gh.Actions.CreateWorkflowDispatchEventByID(ctx, req.Repo.Owner, req.Repo.Name, workflowID, body)
		})
	elapsed := time.Since(started)

	if err != nil {
		c.metrics.IncCounter("gateway_github_dispatch_total", map[string]string{
			"repo": req.Repo.String(), "workflow": req.Workflow, "outcome": string(KindOf(err)),
		})
		return nil, err
	}

	c.metrics.IncCounter("gateway_github_dispatch_total", map[string]string{
		"repo": req.Repo.String(), "workflow": req.Workflow, "outcome": "forwarded",
	})

	result := &DispatchResult{
		Plan:                      plan,
		UpstreamStatus:            statusOf(resp),
		UpstreamDuration:          elapsed,
		ReturnRunDetailsRequested: plan.ReturnRunDetails,
	}
	if details != nil {
		result.RunID = details.GetWorkflowRunID()
		result.RunURL = details.GetRunURL()
		result.HTMLURL = details.GetHTMLURL()
	}
	return result, nil
}

func toAnyMap(in map[string]string) map[string]any {
	if len(in) == 0 {
		// The dispatch mapping sends an absent inputs object as {}. An explicit empty object is
		// sent rather than omitting the field, so the request shape does not
		// depend on whether the caller supplied inputs.
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// sortStrings keeps unsupported-key lists deterministic so the 422 body and
// the log line agree between runs.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
