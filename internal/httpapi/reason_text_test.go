package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// reason_text on a trigger_rejected row.
//
// The rule is PARITY WITH THE SUCCESS PATH, and it is stated as one sentence:
//
//	a value that would not have been persisted had the request succeeded is not
//	persisted because it failed.
//
// That sentence, not the placement of the gate, is what these tests pin. The
// tempting shape here is a table of (reject_reason, literal expected string)
// rows, and it would be wrong twice over: it would hardcode an answer that
// belongs to the success path, and it would turn the one case that carries the
// whole point — `secret_detected`, which KEEPS the reason when the credential
// was in an input and DROPS it when the credential was the reason — into two
// unrelated rows that a refactor can renumber apart.
//
// So the expected value is not written down. It is MEASURED: the same reason
// is first driven through a request that succeeds, and whatever
// trigger_events.reason_text ends up holding becomes the expectation for every
// rejection carrying that same reason.
//
// The oracle cannot be faked by breaking the gate. The success path writes
// body.Reason straight into the event (trigger.go, reserveAndDispatch) and
// never consults admitReason, so a gate that admitted everything, or nothing,
// moves one side of the comparison and not the other.
//
// Two traps this file is written around:
//
//   - peekReason READS and admitReason DECIDES. Every assertion below is on
//     the value that reached the store, never on what was parsed out of the
//     body, because a test of the read passes with the gate removed.
//   - `rate_limited` is NULL by CONSTRUCTION, not by placement. It is pinned
//     against rejectLogger directly, so it keeps holding if the admission ever
//     moves ahead of step 8.
//
// ---------------------------------------------------------------------------

// reasonFixture is one caller-supplied `reason`.
//
// wantStored duplicates the expected-value matrix, and it is checked
// against the SUCCESS path only — it is there so that the parity comparison
// cannot pass by both sides collapsing to empty. Every rejection assertion
// uses the measured oracle instead.
type reasonFixture struct {
	name       string
	value      string
	wantStored string
	why        string
}

// credentialReason carries a known credential prefix. highEntropyReason carries none and
// is caught by the entropy half of the same heuristic: 24 distinct bytes give
// H/log2(n) = 1.0, over the secret scan's 0.9.
const (
	cleanReason        = "release 2.1 for customer ACME"
	credentialReason   = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	highEntropyReason  = "abcdefghijklmnopqrstuvwx"
	overLongReasonRune = "é"
)

func reasonFixtures() []reasonFixture {
	return []reasonFixture{
		{
			name:       "clean",
			value:      cleanReason,
			wantStored: cleanReason,
			why:        "an ordinary reason clears the field rules and the secret scan, so both row types keep it",
		},
		{
			name:       "empty",
			value:      "",
			wantStored: "",
			why:        "the store's nullable() collapses \"\" to NULL for both row types (NULL meaning \"the caller sent no reason\")",
		},
		{
			// 1025 runes, 2050 bytes: over the field's rune limit and nowhere near
			// max_body_bytes, so the request dies at the reason and not at
			// step 2.
			name:       "over 1024 runes",
			value:      strings.Repeat(overLongReasonRune, 1025),
			wantStored: "",
			why:        "the field rules forbid it, so the success path never persists it and neither may a rejection",
		},
		{
			name:       "credential prefix",
			value:      credentialReason,
			wantStored: "",
			why:        "the secret scan keeps this out of the response and the log; the column is not a safer place for it",
		},
		{
			name:       "high entropy, no prefix",
			value:      highEntropyReason,
			wantStored: "",
			why:        "the entropy half of the secret-scan heuristic, which a prefix-only test would miss",
		},
	}
}

// bodyWithReason builds a trigger body carrying a given reason verbatim.
func bodyWithReason(ref string, inputs map[string]string, reason string) string {
	body := map[string]any{"ref": ref, "reason": reason}
	if inputs != nil {
		body["inputs"] = inputs
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// successPathReasonText is the ORACLE: what a `trigger` row ends up
// holding for this reason, measured by driving a request that is refused by
// nothing except, possibly, the reason itself.
//
// A request that does not reach the reservation persisted no reason at all, so
// the answer for it is "" — that is the counterfactual the invariant is about.
func successPathReasonText(t *testing.T, reason string) string {
	t.Helper()
	h := newDefaultHarness(t)
	resp := h.do(triggerReq{
		path: "/t/acme/core/release.yml",
		body: bodyWithReason("refs/heads/main", map[string]string{}, reason),
	})
	body := decode(t, resp)

	if resp.StatusCode != http.StatusOK {
		if h.store.ReserveCount() != 0 {
			t.Fatalf("oracle: the probe was answered %d but still reserved; the success path "+
				"is no longer a clean measurement of what a `trigger` row would carry (body %v)",
				resp.StatusCode, body)
		}
		return ""
	}
	reserves := h.store.Reserves
	if len(reserves) != 1 {
		t.Fatalf("oracle: want exactly one reservation on the success path, got %d", len(reserves))
	}
	return reserves[0].Event.ReasonText
}

type rejectPath struct {
	name       string
	step       string
	wantStatus int
	wantReject store.RejectReason
	// req builds the request that must be refused, carrying `reason`.
	req   func(reason string) triggerReq
	setup func(*harness)
	// only limits this path to the named fixtures. Empty means every fixture.
	// A path is limited when another step would claim the request first
	// — an over-long reason cannot reach step 13, because step 11 refuses it.
	only []string
}

func rejectPaths() []rejectPath {
	return []rejectPath{
		{
			name:       "caller_policy",
			step:       "step 9",
			wantStatus: http.StatusForbidden,
			wantReject: store.RejectCallerPolicy,
			req: func(reason string) triggerReq {
				// The allowlist permits refs/tags/* here; this caller does
				// not. The refusal is the caller's own policy, and it is the
				// rejection an operator resolves by reading the caller's
				// stated intent — the row that came out blank before.
				return triggerReq{
					path: "/t/acme/core/release.yml",
					body: bodyWithReason("refs/tags/v1", nil, reason),
				}
			},
		},
		{
			name:       "not_in_allowlist",
			step:       "step 10",
			wantStatus: http.StatusForbidden,
			wantReject: store.RejectNotInAllowlist,
			req: func(reason string) triggerReq {
				return triggerReq{
					path: "/t/acme/core/test.yml",
					body: bodyWithReason("refs/heads/feature", nil, reason),
				}
			},
		},
		{
			name:       "default_branch_unknown",
			step:       "step 10 fail-closed",
			wantStatus: http.StatusForbidden,
			wantReject: store.RejectDefaultBranchUnknown,
			setup:      func(h *harness) { h.store.WorkflowErr = store.ErrNotFound },
			req: func(reason string) triggerReq {
				return triggerReq{
					path: "/t/acme/nodefaultrefs/build.yml",
					body: bodyWithReason("refs/heads/main", nil, reason),
				}
			},
		},
		{
			name:       "bad_request from the Idempotency-Key check",
			step:       "step 10a",
			wantStatus: http.StatusBadRequest,
			wantReject: store.RejectBadRequest,
			req: func(reason string) triggerReq {
				// Straddles the old gate: the field rules have not run when this refuses.
				return triggerReq{
					path:    "/t/acme/core/release.yml",
					body:    bodyWithReason("refs/heads/main", nil, reason),
					idemKey: "-",
				}
			},
		},
		{
			name:       "bad_request from validateBody",
			step:       "step 11",
			wantStatus: http.StatusBadRequest,
			wantReject: store.RejectBadRequest,
			req: func(reason string) triggerReq {
				// acme/wild allows every ref, so steps 9 and 10 pass and the
				// honest 400 arrives at step 11 — where the reason may be the
				// very field that failed.
				return triggerReq{
					path: "/t/acme/wild/free.yml",
					body: bodyWithReason("main", nil, reason),
				}
			},
		},
		{
			name:       "secret_detected, the credential in an INPUT",
			step:       "step 13",
			wantStatus: http.StatusBadRequest,
			wantReject: store.RejectSecretDetected,
			// An over-long reason never reaches step 13; step 11 claims it.
			only: []string{"clean", "empty", "credential prefix", "high entropy, no prefix"},
			req: func(reason string) triggerReq {
				return triggerReq{
					path: "/t/acme/core/release.yml",
					body: bodyWithReason("refs/heads/main",
						map[string]string{"deploy_key": credentialReason}, reason),
				}
			},
		},
		{
			name:       "secret_detected, the credential IS the reason",
			step:       "step 13",
			wantStatus: http.StatusBadRequest,
			wantReject: store.RejectSecretDetected,
			// Only a reason the secret scan catches can produce this rejection at all.
			only: []string{"credential prefix", "high entropy, no prefix"},
			req: func(reason string) triggerReq {
				return triggerReq{
					path: "/t/acme/core/release.yml",
					body: bodyWithReason("refs/heads/main", map[string]string{}, reason),
				}
			},
		},
	}
}

// TestRejectedReasonTextMatchesTheSuccessPath is the invariant itself.
//
// For every (reason, rejection) pair: the value on the trigger_rejected row is
// exactly the value the SAME reason would have put on a trigger row. Not a
// literal — the measured one.
//
// The two `secret_detected` paths are one property here, not two rows. The
// same rejection, refusing the same request for the same section of the spec,
// keeps the caller's words when the credential was in an input and drops them
// when the credential was the reason, and neither expectation is written down:
// both fall out of the oracle.
func TestRejectedReasonTextMatchesTheSuccessPath(t *testing.T) {
	for _, f := range reasonFixtures() {
		f := f
		t.Run("reason="+f.name, func(t *testing.T) {
			oracle := successPathReasonText(t, f.value)

			// The oracle is asserted against the handed-over matrix ONCE, so
			// that a success path which quietly stopped persisting anything
			// cannot make every comparison below true by collapsing both
			// sides to "".
			if oracle != f.wantStored {
				t.Fatalf("the SUCCESS path stored %q for this reason, want %q — %s",
					truncateForMsg(oracle), truncateForMsg(f.wantStored), f.why)
			}

			for _, p := range rejectPaths() {
				if !appliesTo(p.only, f.name) {
					continue
				}
				t.Run(p.name, func(t *testing.T) {
					h := newDefaultHarness(t)
					if p.setup != nil {
						p.setup(h)
					}
					resp := h.do(p.req(f.value))
					body := decode(t, resp)

					if resp.StatusCode != p.wantStatus {
						t.Fatalf("%s: status = %d, want %d (body %v)",
							p.step, resp.StatusCode, p.wantStatus, body)
					}
					events := h.store.RejectedEvents()
					if len(events) != 1 {
						t.Fatalf("%s: want exactly one trigger_rejected event, got %d", p.step, len(events))
					}
					ev := events[0]
					if ev.RejectReason != p.wantReject {
						t.Fatalf("%s: reject_reason = %q, want %q — the matrix and the code disagree "+
							"about which refusal this is, which is a finding against the gateway and not "+
							"something this test may reconcile",
							p.step, ev.RejectReason, p.wantReject)
					}

					if ev.ReasonText != oracle {
						t.Errorf("%s: trigger_rejected.reason_text = %q, but the same reason on a "+
							"SUCCESSFUL request is stored as %q.\n"+
							"The trigger_rejected rule is parity with the success path: a value that would not "+
							"have been persisted had the request succeeded is not persisted because "+
							"it failed — and a value that WOULD have been is not dropped either.",
							p.step, truncateForMsg(ev.ReasonText), truncateForMsg(oracle))
					}

					// The row must never be the place a refused value leaks
					// out. The secret scan keeps it out of the response body, and this
					// covers the third surface.
					if f.wantStored == "" && f.value != "" && strings.Contains(ev.ReasonText, f.value) {
						t.Errorf("%s: the refused value reached the column verbatim", p.step)
					}
				})
			}
		})
	}
}

// TestSecretDetectedKeepsTheReasonWhenTheCredentialWasNotTheReason states the
// pair above once more, side by side, because it is the row most likely to be
// "simplified" into a blanket clear on secret_detected.
//
// Both requests are refused by the secret scan, with the same reject_reason and the same
// status. They differ only in WHERE the credential was, and the column differs
// with them.
func TestSecretDetectedKeepsTheReasonWhenTheCredentialWasNotTheReason(t *testing.T) {
	inInput := newDefaultHarness(t)
	respA := inInput.do(triggerReq{
		path: "/t/acme/core/release.yml",
		body: bodyWithReason("refs/heads/main",
			map[string]string{"deploy_key": credentialReason}, cleanReason),
	})
	_ = decode(t, respA)

	inReason := newDefaultHarness(t)
	respB := inReason.do(triggerReq{
		path: "/t/acme/core/release.yml",
		body: bodyWithReason("refs/heads/main", map[string]string{}, credentialReason),
	})
	_ = decode(t, respB)

	a, b := oneRejection(t, inInput), oneRejection(t, inReason)
	if a.RejectReason != store.RejectSecretDetected || b.RejectReason != store.RejectSecretDetected {
		t.Fatalf("both refusals must be secret_detected; got %q and %q", a.RejectReason, b.RejectReason)
	}
	if respA.StatusCode != respB.StatusCode {
		t.Fatalf("the two refusals answered %d and %d; they are the same rejection",
			respA.StatusCode, respB.StatusCode)
	}
	if a.ReasonText != cleanReason {
		t.Errorf("credential in an INPUT: reason_text = %q, want the caller's reason %q — "+
			"the reason itself cleared the field rules and the secret scan, so a successful request would have kept it",
			truncateForMsg(a.ReasonText), cleanReason)
	}
	if b.ReasonText != "" {
		t.Errorf("credential IS the reason: reason_text = %q, want NULL — storing it would durably "+
			"keep the suspected credential on the one path that exists to refuse it",
			truncateForMsg(b.ReasonText))
	}
}

// TestCoalescedRateLimitRowCarriesNoReasonByConstruction pins the guarantee at
// the level that OWNS it.
//
// A 429 row is NULL today for two independent reasons: reqCtx admits the
// reason only after step 8 has allowed the request, and Coalesce clears
// the field unconditionally on entry. The first is a property of one line's
// placement in handleTrigger; the second is a property of every 429 row there
// will ever be. Driving 429s through the handler tests the placement and would
// stay green with the clear deleted — so the clear is exercised here directly,
// with a reason on the way in that only Coalesce can remove.
func TestCoalescedRateLimitRowCarriesNoReasonByConstruction(t *testing.T) {
	clock := testutil.NewClock(testutil.BaseTime)
	st := newFakeStore()
	rl := newRejectLogger(st, metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour, clock.Now)

	ev := store.TriggerRejectedEvent{
		CallerTokenID:  "tok_a1",
		CallerName:     "release-bot",
		TargetProject:  "acme/core",
		TargetWorkflow: "release.yml",
		TargetRef:      "refs/heads/main",
		RejectReason:   store.RejectRateLimited,
		HTTPStatus:     http.StatusTooManyRequests,
		ReasonText:     cleanReason,
		RequestID:      "req_0001",
	}
	const n = 3
	for i := 0; i < n; i++ {
		rl.Coalesce(ev)
	}
	rl.Close()

	rows := st.RejectedEvents()
	if len(rows) != 1 {
		t.Fatalf("want one coalesced row, got %d", len(rows))
	}
	if rows[0].SuppressedCount != n {
		t.Fatalf("suppressed_count = %d, want %d", rows[0].SuppressedCount, n)
	}
	if rows[0].ReasonText != "" {
		t.Errorf("a coalesced rate_limited row carries reason_text %q. One row stands for %d "+
			"requests and %d free-text reasons cannot be one string: an operator reading "+
			"suppressed_count next to one sentence takes that sentence as the account of all of "+
			"them. The honest single value is suppressed_count, which is already there.",
			truncateForMsg(rows[0].ReasonText), n, n)
	}

	// The clear belongs to Coalesce and to nothing else: a non-coalesced row
	// written through the same logger keeps its reason. Without this, "always
	// NULL" would also be satisfied by a logger that blanked every row.
	single := ev
	single.RejectReason = store.RejectCallerPolicy
	single.HTTPStatus = http.StatusForbidden
	rl.Write(t.Context(), single)
	rows = st.RejectedEvents()
	if len(rows) != 2 {
		t.Fatalf("want a second, non-coalesced row, got %d rows", len(rows))
	}
	if rows[1].ReasonText != cleanReason {
		t.Errorf("a non-coalesced row lost its reason_text (%q, want %q); the clear is supposed to "+
			"be specific to the coalesced 429 row",
			truncateForMsg(rows[1].ReasonText), cleanReason)
	}
}

// TestRateLimit429RowHasNoReasonEndToEnd is the same guarantee observed from
// outside, through step 8.
//
// It deliberately does NOT assert where the admission runs. Both a gate placed
// after step 8 and one placed before it must produce a NULL here, because
// Coalesce is the thing that decides it.
func TestRateLimit429RowHasNoReasonEndToEnd(t *testing.T) {
	const n = 4
	o := defaultConfigOpts()
	o.perCallerRate = 0.001
	o.perCallerBurst = 1
	o.rejectLogWindow = "1h" // only Close flushes it
	h := newHarness(t, o)

	body := bodyWithReason("refs/heads/main", map[string]string{}, cleanReason)
	warm := h.do(triggerReq{path: "/t/acme/core/release.yml", body: body})
	_ = decode(t, warm)
	if warm.StatusCode != http.StatusOK {
		t.Fatalf("warm-up: status = %d, want 200", warm.StatusCode)
	}
	for i := 0; i < n; i++ {
		resp := h.do(triggerReq{path: "/t/acme/core/release.yml", body: body})
		_ = decode(t, resp)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("request %d: status = %d, want 429", i, resp.StatusCode)
		}
	}
	if err := h.srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := h.store.RejectedEvents()
	if len(rows) != 1 {
		t.Fatalf("want one coalesced row, got %d", len(rows))
	}
	if rows[0].RejectReason != store.RejectRateLimited {
		t.Fatalf("reject_reason = %q, want rate_limited", rows[0].RejectReason)
	}
	// The success path DID store this exact reason on the warm-up request, so
	// this is the one row where parity with the success path is not the rule:
	// A coalesced row is a statement about a window, not about a request.
	if got := h.store.Reserves[0].Event.ReasonText; got != cleanReason {
		t.Fatalf("the warm-up request did not store the reason (%q); the comparison below is "+
			"vacuous without it", truncateForMsg(got))
	}
	if rows[0].ReasonText != "" {
		t.Errorf("a coalesced rate_limited row carries reason_text %q; it stands for %d requests",
			truncateForMsg(rows[0].ReasonText), rows[0].SuppressedCount)
	}
}

// TestRefusedReasonIsSurfacedExactlyOnceAndNeverByValue covers reason_text's third NULL
// meaning (the caller sent a reason and the admission rule refused it), the only one that is invisible: the caller gave a reason and we
// declined to keep it. An operator reading the row concludes "no justification
// given", so the drop is announced.
//
// Two things are pinned, and a third deliberately is not.
//
//   - The WARN carries a cause and never the value. A log line is not a safer
//     home for a suspected credential than a column is.
//   - ONE line per refused value. The gate runs once, at admission; re-deriving
//     the reason at step 11 would announce a single refusal twice, and an
//     operator counting these lines would be counting our code paths.
//
// Not pinned: how many WARNs N requests produce. That number is currently N
// and unbounded, which is reported as a finding rather than frozen here.
func TestRefusedReasonIsSurfacedExactlyOnceAndNeverByValue(t *testing.T) {
	const warnMsg = "reason not recorded on trigger_rejected row"

	t.Run("refused", func(t *testing.T) {
		h := newDefaultHarness(t)
		resp := h.do(triggerReq{
			path: "/t/acme/core/release.yml",
			body: bodyWithReason("refs/tags/v1", nil, credentialReason), // caller_policy
		})
		_ = decode(t, resp)

		lines := h.logs.lines(warnMsg)
		if len(lines) != 1 {
			t.Fatalf("want exactly ONE %q line for one refused value, got %d:\n%s",
				warnMsg, len(lines), strings.Join(lines, "\n"))
		}
		for _, want := range []string{"level=WARN", "cause=", "caller_name=", "request_id="} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("the WARN does not carry %s: %s", want, lines[0])
			}
		}
		if strings.Contains(h.logs.String(), credentialReason) {
			t.Errorf("the refused value was written to the log; the secret scan exists to keep it out of "+
				"exactly this surface:\n%s", h.logs.String())
		}
		if ev := oneRejection(t, h); ev.ReasonText != "" {
			t.Errorf("the WARN announced a drop that did not happen: reason_text = %q",
				truncateForMsg(ev.ReasonText))
		}
	})

	// The rate limiter's shed path stays cheap. A request refused at step 8 is
	// coalesced into a row that carries no reason under any placement, so
	// deciding one for it buys nothing — and announcing the decision would
	// make the log grow per REFUSED REQUEST while the events grow per window,
	// which is the write amplification coalescing exists to prevent, moved
	// one surface over. Measured with the admission ahead of step 8: 25 shed
	// requests produced 26 WARN lines against 2 rows.
	//
	// This is about the LOG, not about the column: the column is NULL either
	// way, which is why the parity tests above stay green across the move and
	// this one does not.
	t.Run("shed at step 8", func(t *testing.T) {
		o := defaultConfigOpts()
		o.perCallerRate = 0.001
		o.perCallerBurst = 1
		o.rejectLogWindow = "1h"
		h := newHarness(t, o)

		body := bodyWithReason("refs/heads/main", map[string]string{}, credentialReason)
		// The warm-up spends the single token. Its own reason IS a credential,
		// so it is refused at step 13 and legitimately warns once; every
		// request after it is shed at step 8 and must add nothing.
		_ = decode(t, h.do(triggerReq{path: "/t/acme/core/release.yml", body: body}))
		before := len(h.logs.lines(warnMsg))

		const n = 12
		for i := 0; i < n; i++ {
			resp := h.do(triggerReq{path: "/t/acme/core/release.yml", body: body})
			_ = decode(t, resp)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("request %d: status = %d, want 429", i, resp.StatusCode)
			}
		}
		if got := len(h.logs.lines(warnMsg)) - before; got != 0 {
			t.Errorf("%d shed requests added %d %q line(s). A 429 row carries no reason_text "+
				"under any placement, so nothing is decided for a shed request and nothing is "+
				"announced: the log must not grow per refused request while the trigger_rejected events grow "+
				"per window.", n, got, warnMsg)
		}
	})

	t.Run("admitted", func(t *testing.T) {
		h := newDefaultHarness(t)
		resp := h.do(triggerReq{
			path: "/t/acme/core/release.yml",
			body: bodyWithReason("refs/tags/v1", nil, cleanReason), // caller_policy
		})
		_ = decode(t, resp)

		if lines := h.logs.lines(warnMsg); len(lines) != 0 {
			t.Errorf("a reason that WAS recorded produced %d drop warning(s); the operator is "+
				"being told a field is missing while it sits on the row:\n%s",
				len(lines), strings.Join(lines, "\n"))
		}
		if ev := oneRejection(t, h); ev.ReasonText != cleanReason {
			t.Errorf("reason_text = %q, want %q", truncateForMsg(ev.ReasonText), cleanReason)
		}
	})
}

// TestReasonIsReadFromTheRawBodyNotFromTheDecodedOne pins the conservative
// source. encoding/json saves the first error and keeps decoding, so a body
// rejected at step 11 can still leave a partially-decoded `reason` behind;
// json.Unmarshal over the whole body fails instead and yields "".
//
// The body below has a duplicate `reason`. It is refused for its ref, and the
// question is which of the two spellings — if either — reaches the column.
func TestReasonIsReadFromTheRawBodyNotFromTheDecodedOne(t *testing.T) {
	h := newDefaultHarness(t)
	// Two `reason` keys and an unknown field: validateBody refuses this at
	// step 11 with DisallowUnknownFields, after having decoded `reason`.
	raw := `{"ref":"refs/tags/v1","reason":"` + cleanReason + `","surprise":1}`
	resp := h.do(triggerReq{path: "/t/acme/core/release.yml", body: raw})
	_ = decode(t, resp)

	ev := oneRejection(t, h)
	if ev.RejectReason != store.RejectCallerPolicy {
		t.Fatalf("reject_reason = %q, want caller_policy (step 9 runs before step 11)", ev.RejectReason)
	}
	// The refusal is step 9's, before validateBody ever ran, and the reason is
	// still admitted: admitReason tests WHICH checks the value cleared, never
	// when they ran.
	if ev.ReasonText != cleanReason {
		t.Errorf("reason_text = %q, want %q: a pre-body rejection admits a reason that clears "+
			"the field rules and the secret scan, because a successful request would have stored it",
			truncateForMsg(ev.ReasonText), cleanReason)
	}
}

// --- helpers ---------------------------------------------------------------

func appliesTo(only []string, name string) bool {
	if len(only) == 0 {
		return true
	}
	for _, n := range only {
		if n == name {
			return true
		}
	}
	return false
}

func oneRejection(t *testing.T, h *harness) store.TriggerRejectedEvent {
	t.Helper()
	events := h.store.RejectedEvents()
	if len(events) != 1 {
		t.Fatalf("want exactly one trigger_rejected event, got %d", len(events))
	}
	return events[0]
}

// truncateForMsg keeps a 1025-rune fixture from filling the failure output.
func truncateForMsg(s string) string {
	r := []rune(s)
	if len(r) <= 64 {
		return s
	}
	return fmt.Sprintf("%s… (%d runes)", string(r[:64]), len(r))
}
