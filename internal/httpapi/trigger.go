package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/fatihy101/standpipe/internal/adapter"
	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// triggerResponse is the trigger endpoint's success body.
//
// `run_id` and `run_html_url` are conditional and their ABSENCE IS NOT A
// FAILURE: it means the dispatch response was lost or tier 1 is off,
// and correlation continues in the background. `correlation` is always present
// — it tells the caller which correlation hardware this trigger went out with.
//
// The field is run_html_url and it carries GitHub's html_url. GitHub's
// dispatch response has TWO urls: run_url is the API address and html_url is
// the browser one. Only the browser one is returned and it is named for what
// it is.
type triggerResponse struct {
	Status         string `json:"status"`
	Backend        string `json:"backend"`
	TriggerEventID string `json:"trigger_event_id"`
	Idempotency    string `json:"idempotency"`
	Correlation    string `json:"correlation"`
	RunID          int64  `json:"run_id,omitempty"`
	RunHTMLURL     string `json:"run_html_url,omitempty"`
	RequestID      string `json:"request_id"`
}

// reqCtx is one trigger request's state. It exists so the rejection paths
// cannot forget the event, the counter or the histogram: they all go through
// reject().
type reqCtx struct {
	s   *Server
	w   http.ResponseWriter
	r   *http.Request
	cfg *config.Config

	start     time.Time
	requestID string
	sourceIP  string
	userAgent string

	repo     string
	workflow string
	ref      string

	caller Caller
	ticket *globalTicket

	// labelled becomes true only after step 10 (the allowlist) has PASSED.
	// Until then repo and workflow are caller-chosen strings, and putting
	// them in a metric label would make its cardinality unbounded.
	labelled bool
}

// handleTrigger is the trigger request-processing order, step by step.
//
// The order is not stylistic. Step 6 (the per-IP limit) runs BEFORE step 7
// (authentication) so unauthenticated floods are shed without touching the
// token table or the database; step 8 (the per-caller limit) runs AFTER, so it
// can be attributed to a caller and logged. Reversing them either lets an
// anonymous flood reach the database or makes the caller limit unattributable.
func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	// ONE configuration snapshot for the whole request. A SIGHUP
	// mid-request cannot half-apply — this request finishes under the policy
	// it started with, and the new one applies from the next request.
	cfg := s.reloader.Current()

	t := &reqCtx{
		s:         s,
		w:         w,
		r:         r,
		cfg:       cfg,
		start:     s.now(),
		requestID: requestID(r, s.newRequestID),
		userAgent: truncate(r.Header.Get("User-Agent"), 512),
		repo:      r.PathValue("owner") + "/" + r.PathValue("repo"),
		workflow:  r.PathValue("workflow"),
	}
	// Every response carries X-Request-Id. The value has already been
	// through requestID's charset filter, so it cannot inject a header.
	w.Header().Set("X-Request-Id", t.requestID)

	// ---- step 1: read timeouts. Enforced by http.Server, not here.

	if r.Method != http.MethodPost {
		writeError(w, t.requestID, errMethodNotAllowed())
		return
	}

	// ---- step 2: body size -> 413
	maxBody := cfg.Gateway.MaxBodyBytes
	if r.ContentLength > maxBody {
		writeError(w, t.requestID, errPayloadTooLarge())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	// ---- step 3: Content-Type -> 415
	if !contentTypeOK(r.Header.Get("Content-Type")) {
		writeError(w, t.requestID, errUnsupportedMediaType())
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, t.requestID, errPayloadTooLarge())
			return
		}
		writeError(w, t.requestID, errBadRequest("request body could not be read"))
		return
	}

	// ---- step 4: JSON parse -> 400
	//
	// Syntax only. This runs BEFORE authentication, so its 400 must never
	// reach the database: trigger_rejected rows cover steps 8-13 and nothing
	// earlier.
	if err := decodeSyntax(raw); err != nil {
		writeError(w, t.requestID, errBadRequest(err.Error()))
		return
	}

	// ---- step 5: source IP (rightmost untrusted hop)
	t.sourceIP = clientIP(r, s.trustedProxies)

	// ---- step 6: PRE-AUTH per-IP rate limit -> 429
	//
	// Nothing is written to the database here and nothing ever will be: an
	// unauthenticated request cannot be attributed to a caller, so the record
	// is a counter. This is also why it precedes step 7 — the
	// token table is not consulted for traffic we are already shedding.
	if ok, delay := s.ipLimit.allow(t.sourceIP); !ok {
		s.metrics.IncCounter("gateway_auth_failures_total", nil)
		s.metrics.IncWatchedIP(t.sourceIP, "pre_auth_rate_limited")
		writeError(w, t.requestID, errRateLimited(seconds(delay, 1), ""))
		return
	}

	// ---- step 7: authentication -> 401
	caller, ok := authenticate(cfg, r.Header.Get("Authorization"))
	if !ok {
		s.metrics.IncCounter("gateway_auth_failures_total", nil)
		s.metrics.IncWatchedIP(t.sourceIP, "auth_failure")
		// A 401 carries no detail, and there is no 404 anywhere on this
		// surface: neither response may become an enumeration oracle.
		writeError(w, t.requestID, errUnauthorized())
		return
	}
	t.caller = caller
	s.metrics.IncCounter("gateway_caller_requests_total", map[string]string{"caller": caller.Name})

	// The ref is needed by steps 9 and 10, which run before body validation.
	// trigger_rejected allows target.ref to be NULL before step 11 for
	// exactly this reason; a malformed ref simply fails to match a policy and
	// the honest 400 arrives at step 11 if the policy is a wildcard.
	t.ref = peekRef(raw)

	// ---- step 8: per-caller and global rate limit -> 429
	allowed, delay, ticket := s.callers().allow(caller.Name)
	t.ticket = ticket
	if !allowed {
		t.reject(errRateLimited(seconds(delay, 1), ""), store.RejectRateLimited)
		return
	}
	// From here on the ticket is refunded only on a replay, because replays
	// do not consume the global limit. Every other outcome —
	// including a rejection — keeps it, because the work was really done.

	// ---- step 9: caller policy -> 403
	if !authorizeCaller(caller, t.repo, t.workflow, t.ref) {
		t.reject(errForbidden(DetailCallerPolicy), store.RejectCallerPolicy)
		return
	}

	// ---- step 10: server-side allowlist -> 403
	switch authorizeTarget(cfg.AllowedTargets, t.repo, t.workflow, t.ref) {
	case targetDenied:
		t.reject(errForbidden(DetailNotInAllowlist), store.RejectNotInAllowlist)
		return
	case targetNeedsDefaultBranch:
		// An allowed_targets entry with no `refs` means the default
		// branch only. FAIL CLOSED — if the default branch is not in the
		// workflow cache we cannot evaluate the rule, and 403
		// default_branch_unknown is the answer, never an allow.
		branch, err := s.defaultBranchOf(r.Context(), t.repo, t.workflow)
		if err != nil || branch == "" {
			t.reject(errForbidden(DetailDefaultBranchUnknown), store.RejectDefaultBranchUnknown)
			return
		}
		if t.ref != "refs/heads/"+branch {
			t.reject(errForbidden(DetailNotInAllowlist), store.RejectNotInAllowlist)
			return
		}
	}

	// The target is now known to be one of a configured, finite set. The
	// label-cardinality rule is satisfied from here and only from here.
	t.labelled = true

	// The Idempotency-Key is mandatory and is the reservation's key.
	// It is checked here rather than at step 4 so that a missing header from
	// an authenticated caller is logged as a trigger_rejected.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		t.reject(errBadRequest(err.Error()), store.RejectBadRequest)
		return
	}

	// ---- step 11: body validation -> 400
	body, inputs, err := validateBody(raw)
	if err != nil {
		t.reject(errBadRequest(err.Error()), store.RejectBadRequest)
		return
	}
	t.ref = body.Ref
	// The ref may have been unparseable when steps 9 and 10 ran. Re-check both
	// against the validated value; otherwise a caller could pass the policy
	// with one spelling and dispatch another.
	if !authorizeCaller(caller, t.repo, t.workflow, t.ref) {
		t.reject(errForbidden(DetailCallerPolicy), store.RejectCallerPolicy)
		return
	}
	if authorizeTarget(cfg.AllowedTargets, t.repo, t.workflow, t.ref) == targetDenied {
		t.reject(errForbidden(DetailNotInAllowlist), store.RejectNotInAllowlist)
		return
	}

	// ---- step 12: input validation -> 422, and a reserved input key -> 400
	//
	// Client.Plan reads the workflow cache and decides the injection AND the
	// correlation label in one place. Both are needed before step 14a, which
	// writes them into a row that is never updated.
	eventID := s.newEventID()
	dreq := forge.DispatchRequest{
		Repo:           forge.Repo{Owner: r.PathValue("owner"), Name: r.PathValue("repo")},
		Workflow:       t.workflow,
		Ref:            t.ref,
		Inputs:         inputs,
		TriggerEventID: eventID,
	}
	plan, err := s.forge.Plan(r.Context(), dreq)
	if err != nil {
		if forge.KindOf(err) == forge.KindReservedInputKey {
			// A reserved input key. Deliberately a 400 and NOT the input
			// validation 422: this is the
			// gateway's own decision about a key it reserves, not an
			// upstream-shaped rejection of the workflow's input set.
			t.reject(errBadRequest(DetailReservedInputKey), store.RejectBadRequest)
			return
		}
		s.log.Error("dispatch plan failed", "err", err, "repo", t.repo, "workflow", t.workflow, "request_id", t.requestID)
		// Plan reads the workflow cache, so a failure here can be either
		// upstream or storage. They are NOT interchangeable: a storage read
		// that failed is a 500, and mapping it through upstreamError would
		// blame GitHub for our own database and tell the caller to retry
		// against a backend that is fine.
		var fe *forge.Error
		if errors.As(err, &fe) {
			t.finish(upstreamError(err), store.OutcomeError)
		} else {
			t.finish(storageError(err), store.OutcomeError)
		}
		return
	}
	if plan.Rejected() {
		// Silent dropping is forbidden. The unsupported keys are named.
		t.reject(errUnsupportedInputs(plan.UnsupportedKeys), store.RejectUnsupportedInputs)
		return
	}

	// ---- step 13: secret scan -> 400
	//
	// The KEY is named and the value never is: the message goes into a
	// response body and a log line.
	if detail, found := scanSecrets(inputs, body.Reason); found {
		t.reject(errBadRequest(detail), store.RejectSecretDetected)
		return
	}

	// ---- step 14: reservation, with the trigger event in the same
	//      transaction (step 14a), and step 15's COMMIT inside Reserve.
	t.reserveAndDispatch(idempotencyKey, eventID, raw, body, inputs, dreq, plan)
}

func (t *reqCtx) reserveAndDispatch(
	idempotencyKey, eventID string,
	raw []byte,
	body triggerBody,
	inputs map[string]string,
	dreq forge.DispatchRequest,
	plan forge.DispatchPlan,
) {
	s := t.s
	ctx := t.r.Context()

	// The PROACTIVE free-space check, before anything is written. Its 503 is a
	// different fact from a write that fails: this one says "an operator can
	// fix this, come back", the other says "we may have half-written the log"
	// and is a 500. storageError keeps them apart.
	if err := s.store.CheckFreeSpace(); err != nil {
		s.log.Error("storage check failed", "err", err, "request_id", t.requestID)
		t.finish(storageError(err), store.OutcomeError)
		return
	}

	bodyHash, err := store.BodySHA256(raw)
	if err != nil {
		t.finish(errBadRequest("body could not be canonicalised: "+err.Error()), store.OutcomeRejected)
		return
	}

	// The hash covers the CALLER'S inputs. The injected
	// standby_trigger_id is not included — it is the event id, already on the
	// row, and hashing it would make the value differ between an injected and
	// a non-injected dispatch of the same request.
	canonicalInputs, err := store.CanonicalValue(anyMap(inputs))
	if err != nil {
		t.finish(errBadRequest("inputs could not be canonicalised"), store.OutcomeRejected)
		return
	}
	inputsHash, err := s.store.InputsHash(ctx, canonicalInputs)
	if err != nil {
		s.log.Error("inputs_hash failed", "err", err, "request_id", t.requestID)
		t.finish(storageError(err), store.OutcomeError)
		return
	}

	correlation, err := adapter.Correlation(plan.Correlation)
	if err != nil {
		// Unreachable through forge.PlanDispatch, which always sets one. It
		// is checked anyway because the failure mode is a legal enum value
		// written to a row that is never updated, which produces
		// valid-looking but wrong data.
		s.log.Error("correlation mapping failed", "err", err, "request_id", t.requestID)
		t.finish(errInternal("correlation could not be determined"), store.OutcomeError)
		return
	}
	validation, err := adapter.InputsValidation(plan.InputsValidation)
	if err != nil {
		s.log.Error("inputs validation mapping failed", "err", err, "request_id", t.requestID)
		t.finish(errInternal("inputs validation could not be determined"), store.OutcomeError)
		return
	}

	var requireRaw *string
	if len(body.Require) > 0 && string(body.Require) != "null" {
		v := string(body.Require)
		requireRaw = &v
	}

	ev := store.TriggerEvent{
		EventID:          eventID,
		TS:               s.now(),
		CallerTokenID:    t.caller.TokenID,
		CallerName:       t.caller.Name,
		CallerSourceIP:   t.sourceIP,
		CallerUserAgent:  t.userAgent,
		TargetProject:    t.repo,
		TargetWorkflow:   t.workflow,
		TargetRef:        t.ref,
		InputsHash:       inputsHash,
		InputsValidation: validation,
		Correlation:      correlation,
		RequireRaw:       requireRaw,
		DecisionBackend:  "github",
		DecisionReason:   "observe_mode",
		ReasonText:       body.Reason,
		RequestID:        t.requestID,
	}

	writeStart := s.now()
	res, err := s.store.Reserve(ctx, store.ReserveRequest{
		CallerName:     t.caller.Name,
		CallerTokenID:  t.caller.TokenID,
		IdempotencyKey: idempotencyKey,
		RequestPath:    "POST " + t.r.URL.Path,
		BodySHA256:     bodyHash,
		TTL:            t.cfg.Gateway.IdempotencyTTL.D(),
		Event:          ev,
	})
	s.metrics.ObserveDuration("gateway_sqlite_write_duration_seconds", s.now().Sub(writeStart),
		map[string]string{"table": "idempotency"})
	if err != nil {
		// If durable storage cannot be written, we return 500
		// and forward NOTHING. A dispatch with no record is worse than no
		// dispatch.
		s.log.Error("reservation failed", "err", err, "request_id", t.requestID, "caller_name", t.caller.Name)
		t.finish(storageError(err), store.OutcomeError)
		return
	}

	switch res.Outcome {
	case store.ReserveReplay:
		// Step 8: a replay does not consume the global limit.
		t.ticket.Refund()
		s.metrics.IncCounter("gateway_idempotency_replay_total", nil)
		t.writeReplay(res)
		return

	case store.ReserveInProgress:
		// NEVER dispatch behind an in-flight key.
		t.finish(errIdempotencyInProgress(), store.OutcomeRejected)
		return

	case store.ReserveConflict:
		s.metrics.IncCounter("gateway_idempotency_conflict_total", nil)
		t.finish(errIdempotencyConflict(string(res.ConflictReason)), store.OutcomeRejected)
		return

	case store.ReserveReclaimed:
		// The design wants the run-link resolver run against the ORIGINAL window
		// before this retry dispatches, so a dispatch whose response was lost
		// is completed instead of repeated. internal/forge exposes no
		// single-trigger resolve entry point (Resolver.Sweep is batch and
		// query-driven), so that check cannot be made here. It is an open
		// interface need; until it exists the retry dispatches and
		// the risk of a duplicate run is REAL and is logged rather than
		// hidden.
		s.log.Warn("idempotency key reclaimed from unknown state without the run-link resolver pre-check",
			"request_id", t.requestID, "caller_name", t.caller.Name,
			"event_id", res.EventID, "original_attempt_at", res.OriginalAttemptAt)
	}

	if !res.Outcome.MayDispatch() {
		s.log.Error("unhandled reservation outcome", "outcome", string(res.Outcome), "request_id", t.requestID)
		t.finish(errInternal("unhandled reservation outcome"), store.OutcomeError)
		return
	}

	t.dispatch(res.EventID, idempotencyKey, dreq, plan)
}

// dispatch is steps 16 and 17 of the request-processing order.
func (t *reqCtx) dispatch(eventID, idempotencyKey string, dreq forge.DispatchRequest, plan forge.DispatchPlan) {
	s := t.s

	// The reservation exists. From here the record MUST be completed even if
	// the caller hangs up, so the dispatch does not inherit the request's
	// cancellation: an abandoned request would otherwise leave an in_flight
	// row and, worse, a dispatch GitHub accepted with no outcome written.
	// The budget is shutdown_grace, which is fixed at upstream_timeout plus
	// a margin.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.r.Context()), t.cfg.Gateway.ShutdownGrace.D())
	defer cancel()

	// Step 16: the stamp is written BEFORE the call. The in-flight sweep tells a
	// queued attempt from a dispatched one by it; stamping late makes the
	// sweeper flip live requests to `unknown`.
	if err := s.store.MarkDispatchStarted(ctx, t.caller.Name, idempotencyKey, eventID); err != nil {
		s.log.Error("dispatch_started_at not stamped; nothing was forwarded",
			"err", err, "request_id", t.requestID, "event_id", eventID)
		// Nothing has gone to GitHub. The row stays in_flight and the
		// in-flight sweep will take it to `unknown` — which is the truthful state,
		// because we do not know whether a later attempt dispatched.
		t.finish(storageError(err), store.OutcomeError)
		return
	}

	result, dispatchErr := s.forge.Dispatch(ctx, dreq, plan)

	now := s.now()
	complete := store.CompleteRequest{
		CallerName:     t.caller.Name,
		IdempotencyKey: idempotencyKey,
		EventID:        eventID,
		Outcome: store.TriggerOutcomeEvent{
			TS:             now,
			TriggerEventID: eventID,
		},
	}

	var (
		apiResp  apiError
		respBody any
		success  bool
	)

	if dispatchErr != nil {
		var fe *forge.Error
		if !errors.As(dispatchErr, &fe) {
			fe = &forge.Error{Kind: forge.KindUnknown, Err: dispatchErr}
		}
		apiResp = upstreamError(dispatchErr)

		// Success is decided by the ERROR, never by a numeric HTTP status. Cacheable() is forge's own "GitHub definitively refused
		// this and a retry changes nothing"; LeavesUnknown() is "the dispatch
		// may have landed".
		if fe.Cacheable() {
			complete.FinalState = store.StateComplete
			complete.Outcome.OutcomeStatus = store.OutcomeRejected
		} else {
			// A timeout, a 5xx, an unreachable backend or a throttle
			// leaves the record retryable and caches NO response.
			complete.FinalState = store.StateUnknown
			complete.Outcome.OutcomeStatus = store.OutcomeError
		}
		complete.Outcome.OutcomeReason = string(fe.Kind)
		complete.Outcome.OutcomeHTTPStatus = fe.UpstreamStatus
		respBody = apiResp.body(t.requestID)

		s.metrics.IncCounter("gateway_upstream_errors_total",
			map[string]string{"upstream_status": strconv.Itoa(fe.UpstreamStatus)})
	} else {
		success = true
		complete.FinalState = store.StateComplete
		complete.Outcome.OutcomeStatus = store.OutcomeForwarded
		complete.Outcome.OutcomeReason = ""
		// The wire status is recorded VERBATIM — 200 when tier 1 is
		// armed, 204 when it is not. Both mean forwarded. It is evidence, not
		// a predicate, and nothing here compares it to a number.
		complete.Outcome.OutcomeHTTPStatus = result.UpstreamStatus
		complete.Outcome.UpstreamDuration = result.UpstreamDuration

		resp := triggerResponse{
			Status:         string(store.OutcomeForwarded),
			Backend:        "github",
			TriggerEventID: eventID,
			Idempotency:    string(store.IdempotencyNew),
			Correlation:    string(plan.Correlation),
			RunID:          result.RunID,
			// html_url is the browser address; run_url is the API
			// address and is neither returned nor stored.
			RunHTMLURL: result.HTMLURL,
			RequestID:  t.requestID,
		}
		respBody = resp
		apiResp = apiError{Status: http.StatusOK}

		// Step 16: the tier-1 link. Built by forge.RunLinkFromDispatch
		// so `dispatch_response` is never spelled at this call site, and
		// carried on CompleteRequest so it shares step 17's transaction, so
		// that no state can occur where the dispatch succeeded and the run
		// id was lost.
		if link, ok := forge.RunLinkFromDispatch(eventID, result, now); ok {
			ev, err := adapter.RunLinkEvent(link)
			if err != nil {
				s.log.Error("tier-1 run_link could not be mapped", "err", err, "event_id", eventID)
			} else {
				complete.RunLink = &ev
			}
		}
	}

	body, err := json.Marshal(respBody)
	if err != nil {
		s.log.Error("response body could not be marshalled", "err", err, "event_id", eventID)
		body = nil
	}
	if complete.FinalState == store.StateComplete {
		// Only deterministic outcomes are cached; Complete forces both to
		// NULL when the state is unknown, so this assignment is safe either
		// way, but it is written under the condition it belongs to.
		complete.ResponseStatus = apiResp.Status
		complete.ResponseBody = string(body)
	}

	writeStart := s.now()
	cres, cerr := s.store.Complete(ctx, complete)
	s.metrics.ObserveDuration("gateway_sqlite_write_duration_seconds", s.now().Sub(writeStart),
		map[string]string{"table": "idempotency"})
	if cerr != nil {
		// The dispatch may have reached GitHub and we could not record it.
		// This is the failure this system exists to avoid,
		// so it is an error-level log and a 500, never a quiet 200.
		s.log.Error("dispatch result could not be recorded",
			"err", cerr, "event_id", eventID, "request_id", t.requestID,
			"dispatch_error", dispatchErr != nil)
		t.finish(storageError(cerr), store.OutcomeError)
		return
	}
	if !cres.Applied {
		// The reservation guard: another attempt owns the record now. The
		// trigger_outcome event was still written, so the dispatch is in the
		// log; only the idempotency row was left alone.
		s.log.Warn("idempotency record was taken over before this attempt completed",
			"event_id", eventID, "request_id", t.requestID)
	}
	if complete.RunLink != nil && !cres.RunLinkApplied {
		s.log.Info("tier-1 run_link not written: an at-least-as-strong link already exists",
			"event_id", eventID)
	}

	if success {
		t.finishOK(apiResp.Status, respBody)
		return
	}
	t.finish(apiResp, complete.Outcome.OutcomeStatus)
}

// writeReplay answers a repeat request from the cached response.
func (t *reqCtx) writeReplay(res store.ReserveResult) {
	// The stored body is replayed with two changes and no others: the
	// idempotency marker becomes "replay", and request_id is freshly minted
	// because this is a different request. run_id and run_html_url come from
	// the cache exactly as they were — a replay performs no dispatch, so a
	// value the original never obtained is never filled in later.
	var decoded map[string]any
	status := res.ResponseStatus
	if status == 0 {
		status = http.StatusOK
	}
	if err := json.Unmarshal([]byte(res.ResponseBody), &decoded); err != nil || decoded == nil {
		// A complete row with an unreadable body is a storage inconsistency,
		// not a caller error. Saying so beats inventing a success.
		t.s.log.Error("cached idempotent response could not be decoded",
			"err", err, "event_id", res.EventID, "request_id", t.requestID)
		t.finish(errInternal("cached response unreadable"), store.OutcomeError)
		return
	}
	if _, isError := decoded["error"]; !isError {
		decoded["idempotency"] = string(store.IdempotencyReplay)
	}
	decoded["request_id"] = t.requestID

	t.observe(statusLabel(status, decoded))
	writeJSON(t.w, status, decoded)
}

// finish writes an error response and records the metrics for it.
func (t *reqCtx) finish(e apiError, outcome store.OutcomeStatus) {
	t.observe(outcome)
	writeError(t.w, t.requestID, e)
}

// finishOK writes a success response and records the metrics for it.
func (t *reqCtx) finishOK(status int, body any) {
	t.observe(store.OutcomeForwarded)
	writeJSON(t.w, status, body)
}

// reject is steps 8-13: refuse, record a trigger_rejected event,
// and count it.
//
// Every rejection between authentication and reservation goes through here, so
// none of the three surfaces — response, event, counter — can be updated
// without the other two.
func (t *reqCtx) reject(e apiError, reason store.RejectReason) {
	s := t.s
	// Counted PER REQUEST. The event is coalesced for 429s; the
	// difference between this counter and the events is exactly the number of
	// coalesced requests, and it is also where a crash-lost window shows up.
	s.metrics.IncCounter("gateway_trigger_rejected_total",
		map[string]string{"caller": t.caller.Name, "reason": string(reason)})

	ev := store.TriggerRejectedEvent{
		TS:              s.now(),
		CallerTokenID:   t.caller.TokenID,
		CallerName:      t.caller.Name,
		CallerSourceIP:  t.sourceIP,
		CallerUserAgent: t.userAgent,
		TargetProject:   t.repo,
		TargetWorkflow:  t.workflow,
		TargetRef:       t.ref,
		RejectReason:    reason,
		HTTPStatus:      e.Status,
		RequestID:       t.requestID,
	}
	if reason == store.RejectRateLimited {
		// One row per caller per reject_log_window, carrying
		// suppressed_count. A row per refusal would turn the limiter into a
		// write amplifier against single-writer SQLite.
		s.rejects.Coalesce(ev)
	} else {
		s.rejects.Write(t.r.Context(), ev)
	}

	t.finish(e, store.OutcomeRejected)
}

// observe records the request-level metrics.
//
// repo and workflow reach a label ONLY when labelled is true, i.e. only after
// step 10 passed and the target is known to come from the configured
// allowlist. Before that they are strings an authenticated caller chose and
// putting them in a label is an unbounded-cardinality hole.
func (t *reqCtx) observe(outcome store.OutcomeStatus) {
	if !t.labelled {
		return
	}
	labels := map[string]string{"repo": t.repo, "workflow": t.workflow}
	t.s.metrics.ObserveDuration("gateway_trigger_duration_seconds", t.s.now().Sub(t.start), labels)
	t.s.metrics.IncCounter("gateway_trigger_total", map[string]string{
		"repo": t.repo, "workflow": t.workflow, "status": string(outcome),
	})
}

// statusLabel derives the trigger_outcome's outcome_status for a replayed response.
//
// It reads the stored body's own `status` field first and falls back to the
// presence of an `error` key. It never treats the numeric HTTP status as the
// definition of success: with tier 1 armed a forwarded
// dispatch was answered 200 and without it 204, and both are `forwarded`.
func statusLabel(_ int, body map[string]any) store.OutcomeStatus {
	if s, ok := body["status"].(string); ok && s == string(store.OutcomeForwarded) {
		return store.OutcomeForwarded
	}
	if _, isError := body["error"]; isError {
		return store.OutcomeRejected
	}
	return store.OutcomeError
}

// peekRef extracts `ref` from a syntactically valid body without validating
// it. Steps 9 and 10 run before step 11, and trigger_rejected allows target.ref to be
// NULL on a rejection from that window.
func peekRef(raw []byte) string {
	var probe struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Ref
}

// defaultBranchOf reads the default branch from the persistent workflow cache
// ErrNotFound is not an error to swallow: it means the definition was
// never fetched, and the allowlist fails closed on that.
func (s *Server) defaultBranchOf(ctx context.Context, repo, workflow string) (string, error) {
	entry, err := s.store.WorkflowDefinition(ctx, repo, workflow, forge.WorkflowCacheDefaultRef)
	if err != nil {
		return "", err
	}
	return entry.DefaultBranch, nil
}

func anyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
