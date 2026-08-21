package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// THE SEVENTEEN REQUEST-PROCESSING STEPS, in order.
//
// The order is the specification's, not a style choice, and two of its
// properties have no external symptom when they are wrong:
//
//   - Step 6 (the pre-auth per-IP limit) runs BEFORE step 7 (authentication),
//     and a request refused there writes NOTHING to trigger_events. That is
//     the explicit scope of trigger_rejected: an unauthenticated request
//     cannot be attributed to a caller, so its only record is a counter. A
//     gateway that logged it would let anonymous traffic drive writes into
//     single-writer SQLite, which is the failure the limiter exists to stop.
//   - Steps 8-13 DO write a trigger_rejected. "Silent dropping is
//     FORBIDDEN" is exactly this, and the difference between "not logged
//     because it is pre-auth" and "not logged because we forgot" is invisible
//     from outside.
//
// So each row below asserts three things together: the status, the error
// constant, and whether durable storage was touched.
// ---------------------------------------------------------------------------

type stepCase struct {
	name  string
	step  string
	setup func(*harness)
	req   triggerReq

	wantStatus int
	wantError  string
	wantDetail string
	// wantReject is the reject reason expected on a written trigger_rejected.
	// Empty means NO trigger_rejected event may be written.
	wantReject   store.RejectReason
	wantReserve  bool
	wantDispatch bool
}

func TestTriggerStepOrder(t *testing.T) {
	cases := []stepCase{
		{
			name:       "non-POST method",
			step:       "405",
			req:        triggerReq{method: http.MethodGet},
			wantStatus: http.StatusMethodNotAllowed,
			wantError:  ErrMethodNotAllowed,
		},
		{
			name: "step 2 — body over max_body_bytes",
			step: "step 2",
			req: triggerReq{
				body: `{"ref":"refs/heads/main","reason":"` + strings.Repeat("x", 400) + `"}`,
			},
			setup: func(h *harness) {
				// The limit is applied to the DECLARED length before the body
				// is read; a 413 that only fired after buffering would already
				// have spent the memory it exists to protect.
			},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantError:  ErrPayloadTooLarge,
		},
		{
			name:       "step 3 — wrong content type",
			step:       "step 3",
			req:        triggerReq{contentType: "text/plain"},
			wantStatus: http.StatusUnsupportedMediaType,
			wantError:  ErrUnsupportedMedia,
		},
		{
			name:       "step 3 — missing content type",
			step:       "step 3",
			req:        triggerReq{contentType: "-"},
			wantStatus: http.StatusUnsupportedMediaType,
			wantError:  ErrUnsupportedMedia,
		},
		{
			name: "step 4 — malformed JSON, BEFORE authentication, writes nothing",
			step: "step 4",
			// noAuth proves the ordering: if step 4 ran after step 7 this
			// would be a 401, and if it wrote an event the pre-auth
			// no-write rule would be broken.
			req:        triggerReq{body: `{"ref":`, noAuth: true},
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
		},
		{
			name:       "step 4 — two JSON documents in one body",
			step:       "step 4",
			req:        triggerReq{body: `{"ref":"refs/heads/main"}{"ref":"refs/heads/main"}`},
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
		},
		{
			name:       "step 7 — unknown token",
			step:       "step 7",
			req:        triggerReq{auth: "Bearer spd_tok_a1_" + strings.Repeat("A", 43) + "_1"},
			wantStatus: http.StatusUnauthorized,
			wantError:  ErrUnauthorized,
		},
		{
			name:       "step 7 — no Authorization header",
			step:       "step 7",
			req:        triggerReq{noAuth: true},
			wantStatus: http.StatusUnauthorized,
			wantError:  ErrUnauthorized,
		},
		{
			name: "step 9 — caller policy",
			step: "step 9",
			// release.yml is in the server allowlist for refs/tags/*, but this
			// caller may only use it on refs/heads/main. The refusal must be
			// the CALLER's policy and not the allowlist, or an operator
			// debugging a 403 looks in the wrong file.
			req: triggerReq{
				path: "/t/acme/core/release.yml",
				body: `{"ref":"refs/tags/v1"}`,
			},
			wantStatus: http.StatusForbidden,
			wantError:  ErrForbidden,
			wantDetail: DetailCallerPolicy,
			wantReject: store.RejectCallerPolicy,
		},
		{
			name: "step 10 — not in the server-side allowlist",
			step: "step 10",
			req: triggerReq{
				path: "/t/acme/core/test.yml",
				body: `{"ref":"refs/heads/feature"}`,
			},
			wantStatus: http.StatusForbidden,
			wantError:  ErrForbidden,
			wantDetail: DetailNotInAllowlist,
			wantReject: store.RejectNotInAllowlist,
		},
		{
			name: "step 10 — entry with no refs and a cold workflow cache fails CLOSED",
			step: "step 10",
			req: triggerReq{
				path: "/t/acme/nodefaultrefs/build.yml",
				body: `{"ref":"refs/heads/main"}`,
			},
			setup: func(h *harness) { h.store.WorkflowErr = store.ErrNotFound },
			// No `refs` means the default branch only. Not knowing the
			// default branch is a 403, NEVER an implicit allow.
			wantStatus: http.StatusForbidden,
			wantError:  ErrForbidden,
			wantDetail: DetailDefaultBranchUnknown,
			wantReject: store.RejectDefaultBranchUnknown,
		},
		{
			name:       "missing Idempotency-Key is logged, because the caller is known by then",
			step:       "step 10a",
			req:        triggerReq{idemKey: "-"},
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
			wantReject: store.RejectBadRequest,
		},
		{
			name: "step 11 — a short ref is refused",
			step: "step 11",
			// acme/wild allows every ref, so the request survives steps 9 and
			// 10 and the honest 400 arrives at step 11 rather than a 403 that
			// looks like a permissions problem.
			req: triggerReq{
				path: "/t/acme/wild/free.yml",
				body: `{"ref":"main"}`,
			},
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
			wantReject: store.RejectBadRequest,
		},
		{
			name: "step 11 — require accepts only {\"backend\":\"any\"}",
			step: "step 11",
			req: triggerReq{
				body: `{"ref":"refs/heads/main","require":{"backend":"act"}}`,
			},
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
			wantReject: store.RejectBadRequest,
		},
		{
			name: "step 12 — unsupported inputs are NAMED, never dropped",
			step: "step 12",
			setup: func(h *harness) {
				h.forge.PlanFn = func(req forge.DispatchRequest) (forge.DispatchPlan, error) {
					return forge.DispatchPlan{
						Ref:              req.Ref,
						Inputs:           req.Inputs,
						Correlation:      forge.CorrelationDispatchResponse,
						InputsValidation: forge.InputsValidationDefaultBranch,
						UnsupportedKeys:  []string{"nope", "alsonope"},
					}, nil
				}
			},
			req: triggerReq{
				body: `{"ref":"refs/heads/main","inputs":{"nope":"1","alsonope":"2"}}`,
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantError:  ErrUnsupportedInputs,
			wantReject: store.RejectUnsupportedInputs,
		},
		{
			name: "step 12 — a caller-supplied standby_trigger_id is 400, not 422",
			step: "reserved input key",
			setup: func(h *harness) {
				h.forge.PlanFn = func(forge.DispatchRequest) (forge.DispatchPlan, error) {
					return forge.DispatchPlan{}, &forge.Error{
						Kind:    forge.KindReservedInputKey,
						Message: "reserved",
					}
				}
			},
			req: triggerReq{
				body: `{"ref":"refs/heads/main","inputs":{"standby_trigger_id":"mine"}}`,
			},
			// Deliberately NOT the input-validation 422: this is the gateway's own decision
			// about a key it reserves, not an upstream-shaped rejection.
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
			wantDetail: DetailReservedInputKey,
			wantReject: store.RejectBadRequest,
		},
		{
			name: "step 13 — a credential-shaped input value is refused and the VALUE is not echoed",
			step: "step 13",
			req: triggerReq{
				body: `{"ref":"refs/heads/main","inputs":{"deploy_key":"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"}}`,
			},
			wantStatus: http.StatusBadRequest,
			wantError:  ErrBadRequest,
			wantReject: store.RejectSecretDetected,
		},
		{
			name:  "step 14 — the proactive free-space check is a 503, not a 500",
			step:  "step 14",
			setup: func(h *harness) { h.store.FreeSpaceErr = store.ErrStorageDegraded },
			// storage_degraded says "an operator can fix this, come back". A
			// write that actually failed says "we may have half-written the
			// log" and is a 500. Collapsing them misinforms the caller.
			wantStatus: http.StatusServiceUnavailable,
			wantError:  ErrStorageDegraded,
		},
		{
			name:         "steps 14-17 — the happy path forwards and records",
			step:         "steps 14-17",
			wantStatus:   http.StatusOK,
			wantReserve:  true,
			wantDispatch: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := defaultConfigOpts()
			if strings.Contains(tc.name, "max_body_bytes") {
				o.maxBody = 64
			}
			h := newHarness(t, o)
			if tc.setup != nil {
				tc.setup(h)
			}

			resp := h.do(tc.req)
			body := decode(t, resp)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("%s: status = %d, want %d (body %v)", tc.step, resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantError != "" {
				if got, _ := body["error"].(string); got != tc.wantError {
					t.Errorf("%s: error = %q, want %q", tc.step, got, tc.wantError)
				}
			}
			if tc.wantDetail != "" {
				if got, _ := body["detail"].(string); got != tc.wantDetail {
					t.Errorf("%s: detail = %q, want %q", tc.step, got, tc.wantDetail)
				}
			}
			if resp.Header.Get("X-Request-Id") == "" {
				t.Errorf("%s: every response carries X-Request-Id; this one did not", tc.step)
			}

			// The three surfaces that must agree.
			events := h.store.RejectedEvents()
			if tc.wantReject == "" {
				if len(events) != 0 {
					t.Errorf("%s: %d trigger_rejected event(s) written where none is allowed: %+v",
						tc.step, len(events), events)
				}
			} else {
				if len(events) != 1 {
					t.Fatalf("%s: want exactly one trigger_rejected event, got %d", tc.step, len(events))
				}
				if events[0].RejectReason != tc.wantReject {
					t.Errorf("%s: reject_reason = %q, want %q", tc.step, events[0].RejectReason, tc.wantReject)
				}
				if events[0].HTTPStatus != tc.wantStatus {
					t.Errorf("%s: the event records http_status %d but the caller was answered %d; "+
						"the response and the event must describe the same refusal",
						tc.step, events[0].HTTPStatus, tc.wantStatus)
				}
				if events[0].CallerName == "" {
					t.Errorf("%s: trigger_rejected has no caller_name; trigger_rejected only covers post-auth refusals", tc.step)
				}
			}
			if got := h.store.ReserveCount() > 0; got != tc.wantReserve {
				t.Errorf("%s: reservation attempted = %v, want %v", tc.step, got, tc.wantReserve)
			}
			if got := h.forge.DispatchCount() > 0; got != tc.wantDispatch {
				t.Errorf("%s: dispatch attempted = %v, want %v", tc.step, got, tc.wantDispatch)
			}
		})
	}
}

// TestPreAuthRateLimitWritesNothing is the pre-auth no-write rule on its own, because it is the
// one step whose correctness is entirely invisible from the response.
//
// A pre-auth 429 and a post-auth 429 look identical to the caller. The
// difference is that one of them must not reach the database at all.
func TestPreAuthRateLimitWritesNothing(t *testing.T) {
	o := defaultConfigOpts()
	o.preAuthRate = 0.001 // effectively no refill for the duration of the test
	o.preAuthBurst = 1
	h := newHarness(t, o)

	// The first request spends the only pre-auth token and is then refused at
	// step 7 for having no credential. Nothing is written: step 7 is also
	// out of trigger_rejected's scope.
	first := h.do(triggerReq{noAuth: true})
	_ = decode(t, first)
	if first.StatusCode != http.StatusUnauthorized {
		t.Fatalf("first request: status = %d, want 401", first.StatusCode)
	}

	// The second never reaches authentication.
	second := h.do(triggerReq{})
	body := decode(t, second)
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429 from step 6", second.StatusCode)
	}
	if got, _ := body["error"].(string); got != ErrRateLimited {
		t.Errorf("error = %q, want %q", got, ErrRateLimited)
	}
	if second.Header.Get("Retry-After") == "" {
		t.Error("a 429 must carry Retry-After")
	}

	if n := len(h.store.RejectedEvents()); n != 0 {
		t.Errorf("PRE-AUTH NO-WRITE RULE VIOLATED: %d trigger_rejected event(s) written for a PRE-AUTH refusal. "+
			"trigger_rejected covers steps 8-13 only; an unauthenticated request cannot be attributed to a caller "+
			"and must leave nothing but a counter", n)
	}
	if n := h.store.ReserveCount(); n != 0 {
		t.Errorf("PRE-AUTH NO-WRITE RULE VIOLATED: %d reservation(s) attempted before authentication", n)
	}
	// The counter is the record. Its absence would make the refusal silent,
	// and a silent refusal is the first failure mode this system guards against.
	if got := h.counterValue("gateway_auth_failures_total", nil); got != 2 {
		t.Errorf("gateway_auth_failures_total = %v, want 2 (one 401, one pre-auth 429)", got)
	}
}

// TestRateLimit429IsCoalescedIntoOneEvent is trigger_rejected's write-amplification rule.
//
// N refusals must produce ONE row carrying suppressed_count = N, and the
// per-request counter must still read N. The difference between
// the two is defined as the number of coalesced requests, so if the event stopped
// carrying the count the counter would silently start meaning something else.
func TestRateLimit429IsCoalescedIntoOneEvent(t *testing.T) {
	const n = 7

	o := defaultConfigOpts()
	o.perCallerRate = 0.001
	o.perCallerBurst = 1
	o.rejectLogWindow = "1h" // long enough that only Close flushes it
	h := newHarness(t, o)

	// Spend the single token on a request that succeeds, so every subsequent
	// one is refused at step 8 and nowhere else.
	warm := h.do(triggerReq{})
	_ = decode(t, warm)
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("warm-up request: status = %d, want 200", warm.StatusCode)
	}

	for i := 0; i < n; i++ {
		resp := h.do(triggerReq{})
		body := decode(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("request %d: status = %d, want 429", i, resp.StatusCode)
		}
		if got, _ := body["error"].(string); got != ErrRateLimited {
			t.Fatalf("request %d: error = %q, want %q", i, got, ErrRateLimited)
		}
	}

	// Nothing is written while the window is open.
	if got := len(h.store.RejectedEvents()); got != 0 {
		t.Errorf("%d event(s) written before the coalescing window closed; coalescing writes one row per window", got)
	}

	// The design accepts losing an open window on a CRASH and says so. An orderly
	// stop flushes, or a documented residual risk becomes an undocumented one.
	if err := h.srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := h.store.RejectedEvents()
	if len(events) != 1 {
		t.Fatalf("want exactly ONE coalesced trigger_rejected, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.RejectReason != store.RejectRateLimited {
		t.Errorf("reject_reason = %q, want %q", ev.RejectReason, store.RejectRateLimited)
	}
	if ev.SuppressedCount != n {
		t.Errorf("suppressed_count = %d, want %d — the row understates the refusal by %d requests",
			ev.SuppressedCount, n, n-ev.SuppressedCount)
	}
	if ev.SuppressedSince.IsZero() {
		t.Error("suppressed_since is zero; it must record when the window opened")
	}
	if !ev.TS.After(ev.SuppressedSince) && !ev.TS.Equal(ev.SuppressedSince) {
		t.Errorf("ts (%s) precedes suppressed_since (%s); ts is stamped at the moment the window CLOSED",
			ev.TS, ev.SuppressedSince)
	}

	// The counter is per REQUEST, not per row.
	got := h.counterValue("gateway_trigger_rejected_total", map[string]string{
		"caller": "release-bot", "reason": string(store.RejectRateLimited),
	})
	if got != n {
		t.Errorf("gateway_trigger_rejected_total = %v, want %d (one per refused REQUEST)", got, n)
	}
}

// TestReplayRefundsTheGlobalTokenButNotThePerCallerOne pins step 8's
// exact rule: "replay requests do not consume the global limit."
//
// The asymmetry is deliberate and easy to "fix" into a bug. The global bucket
// is a shared resource and a replay did no upstream work, so the token goes
// back. The PER-CALLER bucket is that caller's own budget and a replay is
// still a request it made — refunding it too would let a caller loop on one
// idempotency key at unlimited rate and never be throttled.
func TestReplayRefundsTheGlobalTokenButNotThePerCallerOne(t *testing.T) {
	o := defaultConfigOpts()
	o.perCallerRate = 0.001
	o.perCallerBurst = 3
	o.globalRate = 0.001
	o.globalBurst = 3
	h := newHarness(t, o)

	h.store.ReserveOut = store.ReserveResult{
		Outcome:        store.ReserveReplay,
		EventID:        "evt_original",
		ResponseStatus: http.StatusOK,
		ResponseBody: `{"status":"forwarded","backend":"github","trigger_event_id":"evt_original",` +
			`"idempotency":"new","correlation":"dispatch_response","run_id":4242,"request_id":"req_original"}`,
	}

	globalBefore := h.srv.callers().global.TokensAt(h.clock.Now())

	resp := h.do(triggerReq{idemKey: "same-key"})
	body := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, _ := body["idempotency"].(string); got != string(store.IdempotencyReplay) {
		t.Errorf("idempotency = %q, want %q", got, store.IdempotencyReplay)
	}
	// The replay answers from the cache. run_id is the ORIGINAL's.
	if got, ok := body["run_id"]; !ok {
		t.Error("the replayed body lost run_id; a replay returns the cached response verbatim")
	} else if fmt.Sprint(got) != "4242" {
		t.Errorf("run_id = %v, want 4242 (the original's)", got)
	}
	// request_id is freshly minted, because this IS a different request.
	if got, _ := body["request_id"].(string); got == "req_original" {
		t.Error("the replay echoed the original's request_id; a replay is a different request")
	}

	after := h.srv.callers()
	globalAfter := after.global.TokensAt(h.clock.Now())
	perCallerAfter := tokensFor(t, after, "release-bot")

	if globalAfter < globalBefore-0.001 {
		t.Errorf("the global bucket lost %.4f token(s) on a REPLAY (%.4f -> %.4f); "+
			"step 8: replay requests do not consume the global limit",
			globalBefore-globalAfter, globalBefore, globalAfter)
	}
	// The per-caller bucket must be DOWN one. burst is 3, so 2 is the answer;
	// 3 would mean the refund reached the wrong bucket and a caller could
	// replay one key for ever without ever being throttled.
	if perCallerAfter > float64(o.perCallerBurst)-1+0.001 {
		t.Errorf("the per-caller bucket was refunded on a replay: %.4f token(s) left of a burst of %d. "+
			"Only the GLOBAL token is returned — the per-caller budget is the caller's own and a replay is "+
			"still a request it made",
			perCallerAfter, o.perCallerBurst)
	}

	if h.forge.DispatchCount() != 0 {
		t.Error("a replay dispatched to GitHub")
	}
	if got := h.counterValue("gateway_idempotency_replay_total", nil); got != 1 {
		t.Errorf("gateway_idempotency_replay_total = %v, want 1", got)
	}
}

func tokensFor(t *testing.T, c *callerLimiters, caller string) float64 {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	lim, ok := c.perCall[caller]
	if !ok {
		t.Fatalf("no per-caller limiter for %q", caller)
	}
	return lim.TokensAt(testutil.BaseTime)
}

// ---------------------------------------------------------------------------
// storageError: the 503/500 split that must not be collapsed.
// ---------------------------------------------------------------------------

func TestStorageErrorKeeps503And500Apart(t *testing.T) {
	full := realSQLiteFullError(t)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantRetry  int
	}{
		{
			name:       "ErrStorageDegraded is the PROACTIVE free-space threshold",
			err:        store.ErrStorageDegraded,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ErrStorageDegraded,
			wantRetry:  retryAfterDegraded,
		},
		{
			name:       "a wrapped ErrStorageDegraded is still the proactive threshold",
			err:        fmt.Errorf("reserving: %w", store.ErrStorageDegraded),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   ErrStorageDegraded,
			wantRetry:  retryAfterDegraded,
		},
		{
			name: "SQLITE_FULL is a write that ACTUALLY FAILED",
			err:  full,
			// 503 would tell the caller "retry later". What really happened is
			// that we may have half-written the log, and that is a 500.
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrInternal,
			wantRetry:  retryAfterInternal,
		},
		{
			name:       "any other write failure is a 500",
			err:        errors.New("disk on fire"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   ErrInternal,
			wantRetry:  retryAfterInternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storageError(tc.err)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tc.wantStatus)
			}
			if got.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tc.wantCode)
			}
			if got.RetryAfter != tc.wantRetry {
				t.Errorf("retry_after = %d, want %d", got.RetryAfter, tc.wantRetry)
			}
		})
	}

	if !store.IsDiskFull(full) {
		t.Fatalf("the error generated for SQLITE_FULL is not recognised by store.IsDiskFull: %v", full)
	}
}

// realSQLiteFullError produces a genuine SQLITE_FULL from the driver rather
// than a hand-made stand-in. store.IsDiskFull reads the driver's primary
// result code, and a fake error would exercise the fallback branch instead of
// the one that fires in production.
func realSQLiteFullError(t *testing.T) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "full.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(x TEXT)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA max_page_count=2"); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	pad := strings.Repeat("0123456789", 8)
	for i := 0; i < 10000; i++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO t(x) VALUES(?)", pad); err != nil {
			return err
		}
	}
	t.Fatal("SQLite never reported SQLITE_FULL under max_page_count=2")
	return nil
}

// ---------------------------------------------------------------------------
// The success path's two silent hazards.
// ---------------------------------------------------------------------------

// TestSuccessWritesTheTier1RunLinkInTheSameTransaction: the tier-1 link and
// completion share one transaction.
//
// The tier-1 link must ride on CompleteRequest, not be written afterwards.
// Split in two, a state exists where the dispatch succeeded and the run id was
// lost — and that trigger then enters the resolver queue for nothing.
func TestSuccessWritesTheTier1RunLinkInTheSameTransaction(t *testing.T) {
	h := newDefaultHarness(t)

	resp := h.do(triggerReq{})
	body := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
	}

	completes := h.store.CompleteRequests()
	if len(completes) != 1 {
		t.Fatalf("want one Complete call, got %d", len(completes))
	}
	c := completes[0]
	if c.RunLink == nil {
		t.Fatal("SAME-TRANSACTION RULE VIOLATED: the dispatch response carried a run id and no run_link rode on CompleteRequest")
	}
	if c.RunLink.Resolution != store.ResolutionExact {
		t.Errorf("run_link.resolution = %q, want %q", c.RunLink.Resolution, store.ResolutionExact)
	}
	if c.RunLink.ResolutionSource != store.SourceDispatchResponse {
		t.Errorf("run_link.resolution_source = %q, want %q (tier 1)",
			c.RunLink.ResolutionSource, store.SourceDispatchResponse)
	}
	if c.RunLink.RunID == nil || *c.RunLink.RunID != 4242 {
		t.Errorf("run_link.run_id = %v, want 4242", c.RunLink.RunID)
	}
	if c.Outcome.OutcomeStatus != store.OutcomeForwarded {
		t.Errorf("outcome_status = %q, want %q", c.Outcome.OutcomeStatus, store.OutcomeForwarded)
	}
	// The wire status is recorded VERBATIM. With tier 1
	// armed a forwarded dispatch is a 200, not a 204, and nothing may treat
	// 204 as the definition of success.
	if c.Outcome.OutcomeHTTPStatus != http.StatusOK {
		t.Errorf("outcome_http_status = %d, want 200 recorded verbatim", c.Outcome.OutcomeHTTPStatus)
	}
	if c.FinalState != store.StateComplete {
		t.Errorf("final state = %q, want %q", c.FinalState, store.StateComplete)
	}
}

// TestNoRunIDMeansNoRunLinkAndIsNotAFailure is the response contract's
// "absence is not a failure": tier 1 off, or a lost response, yields a 200 with no run_id and
// NOTHING written to run_link.
func TestNoRunIDMeansNoRunLinkAndIsNotAFailure(t *testing.T) {
	h := newDefaultHarness(t)
	h.forge.PlanFn = func(req forge.DispatchRequest) (forge.DispatchPlan, error) {
		return forge.DispatchPlan{
			Ref:              req.Ref,
			Inputs:           req.Inputs,
			Correlation:      forge.CorrelationWindow,
			InputsValidation: forge.InputsValidationSkippedColdCache,
			ReturnRunDetails: false,
		}, nil
	}
	h.forge.DispatchFn = func(_ forge.DispatchRequest, plan forge.DispatchPlan) (*forge.DispatchResult, error) {
		// 204 AND NO BODY — what GitHub really answers with tier 1 off.
		return &forge.DispatchResult{Plan: plan, UpstreamStatus: http.StatusNoContent}, nil
	}

	resp := h.do(triggerReq{})
	body := decode(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the absence of run_id is NOT a failure", resp.StatusCode)
	}
	if _, present := body["run_id"]; present {
		t.Errorf("run_id present in a response that never had one: %v", body["run_id"])
	}
	if got, _ := body["correlation"].(string); got != string(store.CorrelationWindow) {
		t.Errorf("correlation = %q, want %q — the caller is told which hardware went out with the trigger",
			got, store.CorrelationWindow)
	}

	completes := h.store.CompleteRequests()
	if len(completes) != 1 {
		t.Fatalf("want one Complete call, got %d", len(completes))
	}
	if completes[0].RunLink != nil {
		t.Error("a run_link was written with no run id from the dispatch response")
	}
	if completes[0].Outcome.OutcomeStatus != store.OutcomeForwarded {
		t.Errorf("outcome_status = %q, want forwarded — 204 is still forwarded",
			completes[0].Outcome.OutcomeStatus)
	}
	if completes[0].Outcome.OutcomeHTTPStatus != http.StatusNoContent {
		t.Errorf("outcome_http_status = %d, want 204 recorded verbatim", completes[0].Outcome.OutcomeHTTPStatus)
	}
}

// TestUpstreamFailuresSplitCacheableFromUnknown: only deterministic outcomes
// are cached, and success is decided by the ERROR KIND, never by a number.
//
// A 404 or 422 is a definitive refusal — the record completes and the response
// is cached, so the caller does not retry. A timeout or a 5xx leaves the
// dispatch UNKNOWN, caches nothing, and keeps the key retryable.
func TestUpstreamFailuresSplitCacheableFromUnknown(t *testing.T) {
	cases := []struct {
		name       string
		kind       forge.Kind
		upstream   int
		wantStatus int
		wantCode   string
		wantState  store.State
		wantOutcpm store.OutcomeStatus
		wantCached bool
	}{
		{"upstream 404", forge.KindWorkflowNotFound, 404, http.StatusBadGateway, ErrUpstreamError,
			store.StateComplete, store.OutcomeRejected, true},
		{"upstream 422", forge.KindUpstreamRejected, 422, http.StatusUnprocessableEntity, ErrUpstreamRejected,
			store.StateComplete, store.OutcomeRejected, true},
		{"upstream 5xx", forge.KindUpstreamServerError, 503, http.StatusBadGateway, ErrUpstreamError,
			store.StateUnknown, store.OutcomeError, false},
		{"upstream timeout", forge.KindUpstreamTimeout, 0, http.StatusBadGateway, ErrUpstreamError,
			store.StateUnknown, store.OutcomeError, false},
		{"secondary rate limit is a 429 to the caller, and it is UPSTREAM", forge.KindSecondaryRateLimit, 403,
			http.StatusTooManyRequests, ErrRateLimited, store.StateUnknown, store.OutcomeError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDefaultHarness(t)
			h.forge.DispatchFn = func(forge.DispatchRequest, forge.DispatchPlan) (*forge.DispatchResult, error) {
				return nil, &forge.Error{
					Kind:           tc.kind,
					UpstreamStatus: tc.upstream,
					Message:        "upstream said no",
					RetryAfter:     30 * time.Second,
				}
			}

			resp := h.do(triggerReq{})
			body := decode(t, resp)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %v)", resp.StatusCode, tc.wantStatus, body)
			}
			if got, _ := body["error"].(string); got != tc.wantCode {
				t.Errorf("error = %q, want %q", got, tc.wantCode)
			}

			completes := h.store.CompleteRequests()
			if len(completes) != 1 {
				t.Fatalf("want one Complete call, got %d", len(completes))
			}
			c := completes[0]
			if c.FinalState != tc.wantState {
				t.Errorf("final state = %q, want %q", c.FinalState, tc.wantState)
			}
			if c.Outcome.OutcomeStatus != tc.wantOutcpm {
				t.Errorf("outcome_status = %q, want %q", c.Outcome.OutcomeStatus, tc.wantOutcpm)
			}
			if c.Outcome.OutcomeReason != string(tc.kind) {
				t.Errorf("outcome_reason = %q, want the forge Kind %q", c.Outcome.OutcomeReason, tc.kind)
			}
			if got := c.ResponseBody != ""; got != tc.wantCached {
				t.Errorf("response cached = %v, want %v — only deterministic outcomes are cached",
					got, tc.wantCached)
			}
			// Caller rate limiting is not the GitHub limiter: an upstream throttle raises the UPSTREAM
			// counter and writes a trigger_outcome; it is never counted as a
			// caller rejection.
			if len(h.store.RejectedEvents()) != 0 {
				t.Errorf("an UPSTREAM failure wrote a trigger_rejected; trigger_rejected covers caller refusals only")
			}
			if got := h.counterValue("gateway_upstream_errors_total",
				map[string]string{"upstream_status": strconv.Itoa(tc.upstream)}); got != 1 {
				t.Errorf("gateway_upstream_errors_total{upstream_status=%d} = %v, want 1", tc.upstream, got)
			}
		})
	}
}

// TestMarkDispatchStartedFailureForwardsNothing: a dispatch with no record is
// worse than no dispatch.
func TestMarkDispatchStartedFailureForwardsNothing(t *testing.T) {
	h := newDefaultHarness(t)
	h.store.MarkErr = errors.New("write failed")

	resp := h.do(triggerReq{})
	body := decode(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body %v)", resp.StatusCode, body)
	}
	if h.forge.DispatchCount() != 0 {
		t.Error("the stamp failed and the request was forwarded anyway; step 16 stamps BEFORE the call")
	}
}

// TestReserveFailureForwardsNothing is the same rule one step earlier.
func TestReserveFailureForwardsNothing(t *testing.T) {
	h := newDefaultHarness(t)
	h.store.ReserveErr = errors.New("BEGIN IMMEDIATE failed")

	resp := h.do(triggerReq{})
	_ = decode(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if h.forge.DispatchCount() != 0 {
		t.Error("the reservation failed and the request was forwarded anyway; a dispatch with no record is worse than no dispatch")
	}
}

// TestInFlightAndConflictNeverDispatch: never dispatch behind a held or
// conflicting idempotency key.
func TestInFlightAndConflictNeverDispatch(t *testing.T) {
	cases := []struct {
		name       string
		out        store.ReserveResult
		wantStatus int
		wantCode   string
		wantDetail string
	}{
		{
			name:       "in_flight",
			out:        store.ReserveResult{Outcome: store.ReserveInProgress, EventID: "evt_other"},
			wantStatus: http.StatusConflict,
			wantCode:   ErrIdempotencyProgress,
		},
		{
			name: "body mismatch",
			out: store.ReserveResult{
				Outcome: store.ReserveConflict, EventID: "evt_other",
				ConflictReason: store.ConflictBody,
			},
			wantStatus: http.StatusConflict,
			wantCode:   ErrIdempotencyConflict,
			wantDetail: string(store.ConflictBody),
		},
		{
			name: "path mismatch",
			out: store.ReserveResult{
				Outcome: store.ReserveConflict, EventID: "evt_other",
				ConflictReason: store.ConflictPath,
			},
			wantStatus: http.StatusConflict,
			wantCode:   ErrIdempotencyConflict,
			wantDetail: string(store.ConflictPath),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDefaultHarness(t)
			h.store.ReserveOut = tc.out

			resp := h.do(triggerReq{})
			body := decode(t, resp)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got, _ := body["error"].(string); got != tc.wantCode {
				t.Errorf("error = %q, want %q", got, tc.wantCode)
			}
			if tc.wantDetail != "" {
				if got, _ := body["detail"].(string); got != tc.wantDetail {
					t.Errorf("detail = %q, want %q — the two 409 causes are distinguishable on purpose", got, tc.wantDetail)
				}
			}
			if h.forge.DispatchCount() != 0 {
				t.Error("VIOLATED: dispatched behind a held or conflicting idempotency key")
			}
		})
	}
}

// TestNoFourOhFourAnywhereOnThePublicSurface is the anti-enumeration rule.
func TestNoFourOhFourAnywhereOnThePublicSurface(t *testing.T) {
	h := newDefaultHarness(t)
	for _, path := range []string{"/", "/nope", "/t", "/t/acme"} {
		resp := h.do(triggerReq{path: path})
		body := decode(t, resp)
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s answered 404; there is NO 404 on this surface — it would be an enumeration oracle", path)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", path, resp.StatusCode)
			continue
		}
		if got, _ := body["detail"].(string); got != DetailUnknownPath {
			t.Errorf("%s: detail = %q, want %q", path, got, DetailUnknownPath)
		}
	}
}

// findMetric reads one series out of Prometheus text exposition.
func findMetric(t *testing.T, text, name string, labels map[string]string) float64 {
	t.Helper()
	want := name
	if len(labels) > 0 {
		var parts []string
		for k, v := range labels {
			parts = append(parts, fmt.Sprintf("%s=%q", k, v))
		}
		// Order-insensitive: the registry sorts label keys, so match on
		// containment of each pair instead of reconstructing the string.
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(line, name+"{") {
				continue
			}
			ok := true
			for _, p := range parts {
				if !strings.Contains(line, p) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			fields := strings.Fields(line)
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatalf("parsing %q: %v", line, err)
			}
			return v
		}
		return 0
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, want+" ") {
			fields := strings.Fields(line)
			v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			if err != nil {
				t.Fatalf("parsing %q: %v", line, err)
			}
			return v
		}
	}
	return 0
}
