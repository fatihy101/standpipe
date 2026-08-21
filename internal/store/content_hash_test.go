package store

import (
	"strings"
	"testing"
	"time"
)

// sampleTrigger is a fully populated trigger event, including all three
// redactable fields, so that Forget has something to remove.
func sampleTrigger(eventID string) TriggerEvent {
	requireRaw := `{"backend":"github"}`
	return TriggerEvent{
		EventID:            eventID,
		TS:                 testBase(),
		OriginForge:        "github",
		Epoch:              0,
		CallerTokenID:      "tok_a1",
		CallerName:         "acme",
		CallerSourceIP:     "203.0.113.7",
		CallerUserAgent:    "acme-deployer/2.1",
		TargetProject:      "acme/core",
		TargetWorkflow:     "release.yml",
		TargetRef:          "refs/heads/main",
		InputsHash:         "hmac-sha256:9f3c1a2b7d4e5f60:abc123",
		InputsValidation:   ValidationDefaultBranch,
		Correlation:        CorrelationInjected,
		IdempotencyKey:     "key-hash",
		IdempotencyOutcome: IdempotencyNew,
		RequireRaw:         &requireRaw,
		DecisionBackend:    "github",
		DecisionReason:     "observation_mode",
		ReasonText:         "release 2.1 for customer ACME, contact ops@example.com",
		RequestID:          "req_hash",
	}
}

// sampleRejected is a fully populated trigger_rejected event, including
// all three redactable fields, so that Forget has something to remove.
func sampleRejected(eventID string) TriggerRejectedEvent {
	return TriggerRejectedEvent{
		EventID:         eventID,
		TS:              testBase(),
		OriginForge:     "github",
		Epoch:           0,
		CallerTokenID:   "tok_a1",
		CallerName:      "acme",
		CallerSourceIP:  "203.0.113.7",
		CallerUserAgent: "acme-deployer/2.1",
		TargetProject:   "acme/core",
		TargetWorkflow:  "release.yml",
		TargetRef:       "refs/heads/main",
		RejectReason:    RejectUnsupportedInputs,
		HTTPStatus:      422,
		ReasonText:      "unknown input 'customer', contact ops@example.com",
		RequestID:       "req_hash",
	}
}

// TestContentSHA256SurvivesForget is the central claim of the trigger and
// trigger_rejected hash scopes: the
// redactable fields are outside BOTH hash scopes, so `standpipe forget`
// can NULL them without invalidating a single content_sha256.
//
// If this breaks, every redacted event in the archive becomes unverifiable and
// Phase 3 absorbs a log it cannot check.
//
// It covers a `trigger` row and a `trigger_rejected` row together, because
// Forget's predicate covers both. Redacting a row type whose hash
// scope still contains a redactable field is exactly the failure this test
// exists to catch, and testing only one of the two types would leave the other
// unguarded.
func TestContentSHA256SurvivesForget(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	// A parentless trigger_rejected rejection: authenticated, then refused before any
	// reservation was taken. NOT a trigger + trigger_outcome pair — that
	// representation was replaced.
	rej := sampleRejected("evt_hash_rej")
	if _, err := env.AppendTriggerRejected(c, rej); err != nil {
		t.Fatalf("AppendTriggerRejected: %v", err)
	}

	// A real `trigger` row, written the way the request path writes it.
	trigReq := reserveReq("acme", "key-hash", "POST /t/acme/core/release.yml", bodyHash("b"), "evt_hash_trig")
	trigReq.Event.TS = testBase()
	trigReq.Event.CallerSourceIP = "203.0.113.7"
	trigReq.Event.CallerUserAgent = "acme-deployer/2.1"
	trigReq.Event.ReasonText = "release 2.1 for customer ACME, contact ops@example.com"
	if _, err := env.Reserve(c, trigReq); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	trig := trigReq.Event
	trig.IdempotencyKey = "key-hash"
	trig.IdempotencyOutcome = IdempotencyNew
	trig.CallerName = "acme"
	trig.CallerTokenID = trigReq.CallerTokenID

	rows := []struct {
		name    string
		eventID string
		// scope recomputes the row's hash scope, optionally with
		// the redactable fields cleared.
		scope func(redacted bool) map[string]any
	}{
		{"trigger", "evt_hash_trig", func(redacted bool) map[string]any {
			ev := trig
			if redacted {
				ev.CallerSourceIP, ev.CallerUserAgent, ev.ReasonText = "", "", ""
			}
			return triggerHashScope(ev)
		}},
		{"trigger_rejected", "evt_hash_rej", func(redacted bool) map[string]any {
			ev := rej
			if redacted {
				ev.CallerSourceIP, ev.CallerUserAgent, ev.ReasonText = "", "", ""
			}
			return triggerRejectedHashScope(ev)
		}},
	}

	before := map[string]string{}
	for _, r := range rows {
		var stored string
		env.rawScan(t, []any{&stored}, `SELECT content_sha256 FROM trigger_events WHERE event_id = ?`, r.eventID)
		if !strings.HasPrefix(stored, "sha256:") {
			t.Fatalf("%s content_sha256 = %q, want the sha256: form", r.name, stored)
		}
		// The stored hash must equal an independent recomputation over the
		// closed scope, not merely equal whatever the export prints.
		recomputed, err := contentSHA256(r.scope(false))
		if err != nil {
			t.Fatalf("contentSHA256: %v", err)
		}
		if recomputed != stored {
			t.Fatalf("%s: stored content_sha256 %s does not match a recomputation %s", r.name, stored, recomputed)
		}
		before[r.eventID] = stored
	}

	env.clock.Set(testBase().Add(90 * 24 * time.Hour))
	n, err := env.ForgetPlan(c, env.clock.Now())
	if err != nil {
		t.Fatalf("ForgetPlan: %v", err)
	}
	if n != 2 {
		t.Fatalf("ForgetPlan = %d, want 2 (trigger + trigger_rejected) — forget redacts both", n)
	}
	n, err = env.Forget(c, env.clock.Now())
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if n != 2 {
		t.Fatalf("Forget = %d rows, want 2", n)
	}

	for _, r := range rows {
		// The redactable fields are gone ...
		var ip, ua, reason any
		env.rawScan(t, []any{&ip, &ua, &reason},
			`SELECT caller_source_ip, caller_user_agent, reason_text FROM trigger_events WHERE event_id = ?`, r.eventID)
		if ip != nil || ua != nil || reason != nil {
			t.Errorf("%s after Forget: source_ip=%v user_agent=%v reason_text=%v, want all NULL", r.name, ip, ua, reason)
		}

		// ... and the hash is untouched, both as stored and as recomputed from
		// the redacted event.
		var after string
		env.rawScan(t, []any{&after}, `SELECT content_sha256 FROM trigger_events WHERE event_id = ?`, r.eventID)
		if after != before[r.eventID] {
			t.Fatalf("%s content_sha256 changed across Forget: %s -> %s", r.name, before[r.eventID], after)
		}
		afterRecomputed, err := contentSHA256(r.scope(true))
		if err != nil {
			t.Fatalf("contentSHA256: %v", err)
		}
		if afterRecomputed != before[r.eventID] {
			t.Fatalf("%s: recomputing the hash of the REDACTED event gives %s, want the stored %s — a redactable field leaked into the hash scope",
				r.name, afterRecomputed, before[r.eventID])
		}
	}

	// Forget is idempotent: a second run has nothing left to redact.
	if n, err := env.Forget(c, env.clock.Now()); err != nil || n != 0 {
		t.Errorf("second Forget = %d rows, err %v; want 0", n, err)
	}
}

// flattenScope turns a hash scope into its dotted key set ("caller.token_id"),
// so an enumeration in a test can be compared against the scope the writer
// actually builds.
func flattenScope(prefix string, scope map[string]any, into map[string]bool) {
	for k, v := range scope {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if nested, ok := v.(map[string]any); ok {
			flattenScope(key, nested, into)
			continue
		}
		into[key] = true
	}
}

// assertScopeIsClosed is the point of this whole file, mechanised.
//
// The sensitivity tables below are hand-written, and a hand-written
// enumeration silently stops being true the day someone adds a field to a hash
// scope: the new field is simply never exercised, and the test keeps passing
// while the guarantee it claims has a hole in it. That is precisely how
// `correlation` sat outside this enumeration for a whole slice.
//
// So instead of trusting the table, compare it against the scope: every key
// the writer emits must be either exercised by the table or listed as a
// deliberate structural constant, and every key the table names must exist.
// Adding a field to a hash scope now fails here, at the field's own name.
func assertScopeIsClosed(t *testing.T, scope map[string]any, exercised []string, constants map[string]string) {
	t.Helper()
	actual := map[string]bool{}
	flattenScope("", scope, actual)

	covered := map[string]bool{}
	for _, k := range exercised {
		if !actual[k] {
			t.Errorf("the table names %q, but the hash scope has no such key — a renamed or removed field", k)
		}
		covered[k] = true
	}
	for k := range constants {
		if !actual[k] {
			t.Errorf("the constant list names %q, but the hash scope has no such key", k)
		}
		covered[k] = true
	}
	for k := range actual {
		if !covered[k] {
			t.Errorf("hash scope key %q is in NEITHER the sensitivity table nor the constant list — "+
				"a new field entered the scope without anyone proving the hash reacts to it", k)
		}
	}
}

// TestContentSHA256ScopeSensitivity walks every in-scope field of the trigger scope and
// asserts the hash moves, then every out-of-scope field and asserts it does
// not. A field silently dropped from the scope would make the log forgeable.
//
// It then asserts the enumeration is COMPLETE, so this test cannot quietly
// stop covering the scope it claims to cover.
func TestContentSHA256ScopeSensitivity(t *testing.T) {
	base := sampleTrigger("evt_scope")
	baseSum, err := contentSHA256(triggerHashScope(base))
	if err != nil {
		t.Fatalf("contentSHA256: %v", err)
	}

	otherRequire := `{"backend":"other"}`
	inScope := map[string]func(*TriggerEvent){
		"event_id":            func(e *TriggerEvent) { e.EventID = "evt_other" },
		"ts":                  func(e *TriggerEvent) { e.TS = base.TS.Add(time.Second) },
		"origin_forge":        func(e *TriggerEvent) { e.OriginForge = "forgejo" },
		"epoch":               func(e *TriggerEvent) { e.Epoch = 1 },
		"caller.token_id":     func(e *TriggerEvent) { e.CallerTokenID = "tok_a2" },
		"caller.name":         func(e *TriggerEvent) { e.CallerName = "beta" },
		"target.project":      func(e *TriggerEvent) { e.TargetProject = "acme/other" },
		"target.workflow":     func(e *TriggerEvent) { e.TargetWorkflow = "deploy.yml" },
		"target.ref":          func(e *TriggerEvent) { e.TargetRef = "refs/tags/v1" },
		"inputs_hash":         func(e *TriggerEvent) { e.InputsHash = "hmac-sha256:0011223344556677:zzz" },
		"inputs_validation":   func(e *TriggerEvent) { e.InputsValidation = ValidationSkippedColdCache },
		"idempotency_key":     func(e *TriggerEvent) { e.IdempotencyKey = "other-key" },
		"idempotency_outcome": func(e *TriggerEvent) { e.IdempotencyOutcome = IdempotencyReplay },
		"require_raw":         func(e *TriggerEvent) { e.RequireRaw = &otherRequire },
		"decision.backend":    func(e *TriggerEvent) { e.DecisionBackend = "local" },
		"decision.reason":     func(e *TriggerEvent) { e.DecisionReason = "routed" },
		"request_id":          func(e *TriggerEvent) { e.RequestID = "req_other" },
		// `correlation` is a CLAIM about what the gateway armed this
		// dispatch with. It cannot be re-derived from the log — an inputs
		// object that was not injected and one where injection was never
		// attempted are identical on the row — so if it left the hash scope,
		// the claim would be forgeable with no way to detect it.
		"correlation": func(e *TriggerEvent) { e.Correlation = CorrelationWindow },
	}
	for name, mutate := range inScope {
		t.Run("in scope/"+name, func(t *testing.T) {
			ev := base
			mutate(&ev)
			sum, err := contentSHA256(triggerHashScope(ev))
			if err != nil {
				t.Fatalf("contentSHA256: %v", err)
			}
			if sum == baseSum {
				t.Fatalf("changing %s did not change content_sha256 — the field is not covered by the trigger scope", name)
			}
		})
	}

	outOfScope := map[string]func(*TriggerEvent){
		"caller.source_ip":  func(e *TriggerEvent) { e.CallerSourceIP = "198.51.100.9" },
		"caller.user_agent": func(e *TriggerEvent) { e.CallerUserAgent = "curl/8.0" },
		"reason_text":       func(e *TriggerEvent) { e.ReasonText = "something else entirely" },
	}
	for name, mutate := range outOfScope {
		t.Run("out of scope/"+name, func(t *testing.T) {
			ev := base
			mutate(&ev)
			sum, err := contentSHA256(triggerHashScope(ev))
			if err != nil {
				t.Fatalf("contentSHA256: %v", err)
			}
			if sum != baseSum {
				t.Fatalf("changing the redactable field %s changed content_sha256 — Forget would invalidate the hash", name)
			}
		})
	}

	// The out-of-scope fields are absent from the scope by construction, so
	// only the in-scope table plus the structural constants may account for it.
	t.Run("scope is closed", func(t *testing.T) {
		exercised := make([]string, 0, len(inScope))
		for k := range inScope {
			exercised = append(exercised, k)
		}
		assertScopeIsClosed(t, triggerHashScope(base), exercised, map[string]string{
			"schema_version": "a constant of the writer, not a field of the event",
			"event_type":     "always 'trigger' in this scope",
			"outcome":        "structurally NULL on a trigger row and never updated",
		})
	})
}

// TestTriggerRejectedHashScopeSensitivity is the same guarantee for trigger_rejected's
// parentless event. It carries the same redactable caller block as a trigger
// row, so Forget must be able to NULL those three fields without invalidating
// the hash — and suppressed_count / suppressed_since must be INSIDE the scope,
// because a coalesced 429 row states how many requests it stands for and that
// number is a claim, not a derivation.
func TestTriggerRejectedHashScopeSensitivity(t *testing.T) {
	base := sampleRejected("evt_rej_scope")
	base.SuppressedCount = 7
	base.SuppressedSince = testBase().Add(-time.Minute)
	base.RejectReason = RejectRateLimited
	baseSum, err := contentSHA256(triggerRejectedHashScope(base))
	if err != nil {
		t.Fatalf("contentSHA256: %v", err)
	}

	inScope := map[string]func(*TriggerRejectedEvent){
		"event_id":            func(e *TriggerRejectedEvent) { e.EventID = "evt_other" },
		"ts":                  func(e *TriggerRejectedEvent) { e.TS = base.TS.Add(time.Second) },
		"origin_forge":        func(e *TriggerRejectedEvent) { e.OriginForge = "forgejo" },
		"epoch":               func(e *TriggerRejectedEvent) { e.Epoch = 1 },
		"caller.token_id":     func(e *TriggerRejectedEvent) { e.CallerTokenID = "tok_a2" },
		"caller.name":         func(e *TriggerRejectedEvent) { e.CallerName = "beta" },
		"target.project":      func(e *TriggerRejectedEvent) { e.TargetProject = "acme/other" },
		"target.workflow":     func(e *TriggerRejectedEvent) { e.TargetWorkflow = "deploy.yml" },
		"target.ref":          func(e *TriggerRejectedEvent) { e.TargetRef = "refs/tags/v1" },
		"reject_reason":       func(e *TriggerRejectedEvent) { e.RejectReason = RejectSecretDetected },
		"outcome_http_status": func(e *TriggerRejectedEvent) { e.HTTPStatus = 429 },
		"suppressed_count":    func(e *TriggerRejectedEvent) { e.SuppressedCount = 8 },
		"suppressed_since":    func(e *TriggerRejectedEvent) { e.SuppressedSince = base.SuppressedSince.Add(time.Second) },
		"request_id":          func(e *TriggerRejectedEvent) { e.RequestID = "req_other" },
	}
	for name, mutate := range inScope {
		t.Run("in scope/"+name, func(t *testing.T) {
			ev := base
			mutate(&ev)
			sum, err := contentSHA256(triggerRejectedHashScope(ev))
			if err != nil {
				t.Fatalf("contentSHA256: %v", err)
			}
			if sum == baseSum {
				t.Fatalf("changing %s did not change content_sha256 — the field is not covered by the trigger_rejected scope", name)
			}
		})
	}

	outOfScope := map[string]func(*TriggerRejectedEvent){
		"caller.source_ip":  func(e *TriggerRejectedEvent) { e.CallerSourceIP = "198.51.100.9" },
		"caller.user_agent": func(e *TriggerRejectedEvent) { e.CallerUserAgent = "curl/8.0" },
		"reason_text":       func(e *TriggerRejectedEvent) { e.ReasonText = "something else entirely" },
	}
	for name, mutate := range outOfScope {
		t.Run("out of scope/"+name, func(t *testing.T) {
			ev := base
			mutate(&ev)
			sum, err := contentSHA256(triggerRejectedHashScope(ev))
			if err != nil {
				t.Fatalf("contentSHA256: %v", err)
			}
			if sum != baseSum {
				t.Fatalf("changing the redactable field %s changed content_sha256 — Forget would invalidate the hash", name)
			}
		})
	}

	t.Run("scope is closed", func(t *testing.T) {
		exercised := make([]string, 0, len(inScope))
		for k := range inScope {
			exercised = append(exercised, k)
		}
		assertScopeIsClosed(t, triggerRejectedHashScope(base), exercised, map[string]string{
			"schema_version": "a constant of the writer",
			"event_type":     "always 'trigger_rejected' in this scope",
		})
	})
}

// TestRunLinkHashScopeSensitivity covers the run_link event, whose scope gained
// `resolution_source`. Two layers both produce `exact`, so the source is the
// only record of WHICH one did; outside the hash it would be rewritable.
func TestRunLinkHashScopeSensitivity(t *testing.T) {
	base := RunLinkEvent{
		EventID: "evt_rl_scope", TS: testBase(), TriggerEventID: "evt_parent",
		RunID: runID(4242), Resolution: ResolutionExact, ResolutionSource: SourceRunName,
		OriginForge: "github",
	}
	baseSum, err := contentSHA256(runLinkHashScope(base))
	if err != nil {
		t.Fatalf("contentSHA256: %v", err)
	}

	inScope := map[string]func(*RunLinkEvent){
		"event_id":          func(e *RunLinkEvent) { e.EventID = "evt_other" },
		"ts":                func(e *RunLinkEvent) { e.TS = base.TS.Add(time.Second) },
		"trigger_event_id":  func(e *RunLinkEvent) { e.TriggerEventID = "evt_other_parent" },
		"run_id":            func(e *RunLinkEvent) { e.RunID = runID(4243) },
		"resolution":        func(e *RunLinkEvent) { e.Resolution = ResolutionInferred },
		"resolution_source": func(e *RunLinkEvent) { e.ResolutionSource = SourceDispatchResponse },
		"origin_forge":      func(e *RunLinkEvent) { e.OriginForge = "forgejo" },
		"epoch":             func(e *RunLinkEvent) { e.Epoch = 1 },
	}
	for name, mutate := range inScope {
		t.Run("in scope/"+name, func(t *testing.T) {
			ev := base
			mutate(&ev)
			sum, err := contentSHA256(runLinkHashScope(ev))
			if err != nil {
				t.Fatalf("contentSHA256: %v", err)
			}
			if sum == baseSum {
				t.Fatalf("changing %s did not change content_sha256 — the field is not covered by the run_link scope", name)
			}
		})
	}

	t.Run("scope is closed", func(t *testing.T) {
		exercised := make([]string, 0, len(inScope))
		for k := range inScope {
			exercised = append(exercised, k)
		}
		assertScopeIsClosed(t, runLinkHashScope(base), exercised, map[string]string{
			"schema_version": "a constant of the writer",
			"event_type":     "always 'run_link' in this scope",
		})
	})
	// superseded_by is deliberately NOT in the scope: it is the one column a
	// run_link row is updated in (the append-only supersede), so including
	// it would invalidate the hash of every superseded link.
	if _, ok := runLinkHashScope(base)["superseded_by"]; ok {
		t.Errorf("superseded_by is inside the run_link hash scope; the supersede UPDATE would then break every old row's content_sha256")
	}
}

// TestContentSHA256Deterministic: two identical events hash identically, and
// the hash does not depend on map iteration order.
func TestContentSHA256Deterministic(t *testing.T) {
	ev := sampleTrigger("evt_det")
	first, err := contentSHA256(triggerHashScope(ev))
	if err != nil {
		t.Fatalf("contentSHA256: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := contentSHA256(triggerHashScope(sampleTrigger("evt_det")))
		if err != nil {
			t.Fatalf("contentSHA256: %v", err)
		}
		if again != first {
			t.Fatalf("content_sha256 is not deterministic: %s vs %s", first, again)
		}
	}
}

// TestReserveWritesVerifiableTriggerEvent ties the request path to the hash:
// the event Reserve writes must itself verify, including the normalisation
// Reserve applies (idempotency_outcome = new, caller fields defaulted).
func TestReserveWritesVerifiableTriggerEvent(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	req := reserveReq("acme", "key-verify", "POST /t/acme/core/release.yml", bodyHash("b"), "evt_verify")
	req.Event.TS = testBase()
	req.Event.CallerSourceIP = "203.0.113.7"
	req.Event.ReasonText = "ship it"
	if _, err := env.Reserve(c, req); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	want := req.Event
	want.IdempotencyKey = "key-verify"
	want.IdempotencyOutcome = IdempotencyNew
	want.CallerName = "acme"
	want.CallerTokenID = req.CallerTokenID
	expected, err := contentSHA256(triggerHashScope(want))
	if err != nil {
		t.Fatalf("contentSHA256: %v", err)
	}

	var stored string
	env.rawScan(t, []any{&stored}, `SELECT content_sha256 FROM trigger_events WHERE event_id = ?`, "evt_verify")
	if stored != expected {
		t.Fatalf("the trigger event Reserve wrote does not verify:\n stored %s\n  want  %s", stored, expected)
	}

	// A trigger row carries no outcome.
	got, err := env.EventByID(c, "evt_verify")
	if err != nil {
		t.Fatalf("EventByID: %v", err)
	}
	if got["outcome"] != nil {
		t.Errorf("trigger event carries outcome %v, want nil", got["outcome"])
	}
	if got["idempotency_outcome"] != string(IdempotencyNew) {
		t.Errorf("idempotency_outcome = %v, want new", got["idempotency_outcome"])
	}
}
