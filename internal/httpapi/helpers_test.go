package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
	"github.com/fatihy101/standpipe/internal/testutil"
)

// These tests live in `package httpapi` rather than `package httpapi_test`
// because the request-processing step ORDER is the thing under test and several of its steps —
// the pre-auth limiter, the reject logger's coalescing window, storageError's
// 503/500 split — are unexported by design. Testing them through the public
// handler is right; testing them ONLY through it would leave the cases that
// have no external symptom uncovered, which is the whole failure class this
// project is trying to close.

// --- configuration ----------------------------------------------------------

// baseConfigYAML is a minimal valid standpipe.yaml. Every knob a test needs to
// move is a %s, so the file that config.Parse validates is the same shape an
// operator writes — a hand-built config.Config would skip validation and
// defaulting, and those are where the configuration rules live.
const baseConfigYAML = `
gateway:
  listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:1"
  max_body_bytes: %d
  idempotency_ttl: "24h"
  upstream_timeout: "10s"
  shutdown_grace: "15s"
  retention_days: 90
  rate_limit:
    per_caller: {rate_per_second: %v, burst: %d}
    global: {rate_per_second: %v, burst: %d}
    pre_auth_ip: {rate_per_second: %v, burst: %d}
    reject_log_window: "%s"
storage:
  sqlite_path: "/tmp/standpipe-test.db"
github:
  app_id: 12345
  private_key_path: "/tmp/standpipe-test.pem"
  max_inflight: 4
  max_queue_wait: "2s"
%s
allowed_targets:
  - repo: "acme/core"
    workflows: ["release.yml", "test.yml"]
    refs: ["refs/heads/main", "refs/tags/*"]
  - repo: "acme/nodefaultrefs"
    workflows: ["build.yml"]
  - repo: "acme/wild"
    workflows: ["free.yml"]
    refs: ["*"]
callers:
  release-bot:
    token_hash: "%s"
    token_id: "%s"
    role: "%s"
    allow:
      - repo: "acme/core"
        workflow: "release.yml"
        ref: "refs/heads/main"
      - repo: "acme/core"
        workflow: "test.yml"
        ref: "*"
      - repo: "acme/nodefaultrefs"
        workflow: "build.yml"
        ref: "*"
      - repo: "acme/wild"
        workflow: "free.yml"
        ref: "*"
`

type configOpts struct {
	maxBody          int64
	perCallerRate    float64
	perCallerBurst   int
	globalRate       float64
	globalBurst      int
	preAuthRate      float64
	preAuthBurst     int
	rejectLogWindow  string
	returnRunDetails *bool
	tokenHash        string
	tokenID          string
	role             string
}

func defaultConfigOpts() configOpts {
	return configOpts{
		maxBody:         65536,
		perCallerRate:   100,
		perCallerBurst:  100,
		globalRate:      100,
		globalBurst:     100,
		preAuthRate:     100,
		preAuthBurst:    100,
		rejectLogWindow: "60s",
		tokenID:         "tok_a1",
		role:            "caller",
	}
}

func buildConfig(t *testing.T, o configOpts) *config.Config {
	t.Helper()
	rrd := ""
	if o.returnRunDetails != nil {
		rrd = fmt.Sprintf("  return_run_details: %t", *o.returnRunDetails)
	}
	raw := fmt.Sprintf(baseConfigYAML,
		o.maxBody,
		o.perCallerRate, o.perCallerBurst,
		o.globalRate, o.globalBurst,
		o.preAuthRate, o.preAuthBurst,
		o.rejectLogWindow,
		rrd,
		o.tokenHash, o.tokenID, o.role,
	)
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("config.Parse: %v\n---\n%s", err, raw)
	}
	cfg.Path = "standpipe.yaml"
	return cfg
}

// configWithCaller builds a configuration carrying one caller.
func configWithCaller(t *testing.T, callerName, tokenID, tokenHash, role string) *config.Config {
	t.Helper()
	o := defaultConfigOpts()
	o.tokenID = tokenID
	o.tokenHash = tokenHash
	o.role = role
	cfg := buildConfig(t, o)
	if callerName != "release-bot" {
		entry := cfg.Callers["release-bot"]
		delete(cfg.Callers, "release-bot")
		cfg.Callers[callerName] = entry
	}
	return cfg
}

// --- fake store -------------------------------------------------------------

// fakeStore is a scripted httpapi.Store. It records what the handler asked it
// to do, which is how "step 6 writes NOTHING to trigger_events"
// becomes an assertion instead of a claim.
type fakeStore struct {
	mu sync.Mutex

	ReserveOut store.ReserveResult
	ReserveErr error
	Reserves   []store.ReserveRequest

	CompleteOut store.CompleteResult
	CompleteErr error
	Completes   []store.CompleteRequest

	MarkErr   error
	MarkCalls int

	InputsHashErr error
	FreeSpaceErr  error
	HealthErr     error

	WorkflowEntry store.WorkflowCacheEntry
	WorkflowErr   error

	Rejected  []store.TriggerRejectedEvent
	RejectErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		CompleteOut: store.CompleteResult{Applied: true, RunLinkApplied: true},
		WorkflowErr: store.ErrNotFound,
	}
}

func (f *fakeStore) Reserve(_ context.Context, req store.ReserveRequest) (store.ReserveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Reserves = append(f.Reserves, req)
	if f.ReserveErr != nil {
		return store.ReserveResult{}, f.ReserveErr
	}
	out := f.ReserveOut
	if out.Outcome == "" {
		out.Outcome = store.ReserveNew
	}
	if out.EventID == "" {
		out.EventID = req.Event.EventID
	}
	return out, nil
}

func (f *fakeStore) MarkDispatchStarted(_ context.Context, _, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.MarkCalls++
	return f.MarkErr
}

func (f *fakeStore) Complete(_ context.Context, req store.CompleteRequest) (store.CompleteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Completes = append(f.Completes, req)
	if f.CompleteErr != nil {
		return store.CompleteResult{}, f.CompleteErr
	}
	return f.CompleteOut, nil
}

func (f *fakeStore) InputsHash(_ context.Context, canonical []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.InputsHashErr != nil {
		return "", f.InputsHashErr
	}
	return fmt.Sprintf("hmac-sha256:k1:%x", len(canonical)), nil
}

func (f *fakeStore) WorkflowDefinition(_ context.Context, _, _, _ string) (store.WorkflowCacheEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.WorkflowErr != nil {
		return store.WorkflowCacheEntry{}, f.WorkflowErr
	}
	return f.WorkflowEntry, nil
}

func (f *fakeStore) CheckFreeSpace() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.FreeSpaceErr
}

func (f *fakeStore) HealthCheck(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.HealthErr
}

func (f *fakeStore) SchemaVersion(context.Context) (int, error) { return 1, nil }

func (f *fakeStore) Coverage(context.Context, int) ([]store.CoverageRow, error) { return nil, nil }
func (f *fakeStore) Observed(context.Context, int) ([]store.ObservedRow, error) { return nil, nil }
func (f *fakeStore) Surface(context.Context) ([]store.SurfaceRow, error)        { return nil, nil }

func (f *fakeStore) AppendTriggerRejected(_ context.Context, ev store.TriggerRejectedEvent) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RejectErr != nil {
		return "", f.RejectErr
	}
	f.Rejected = append(f.Rejected, ev)
	return fmt.Sprintf("evt_rej_%04d", len(f.Rejected)), nil
}

// RejectedEvents returns a copy of the trigger_rejected events written so far.
func (f *fakeStore) RejectedEvents() []store.TriggerRejectedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.TriggerRejectedEvent, len(f.Rejected))
	copy(out, f.Rejected)
	return out
}

func (f *fakeStore) ReserveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Reserves)
}

func (f *fakeStore) CompleteRequests() []store.CompleteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.CompleteRequest, len(f.Completes))
	copy(out, f.Completes)
	return out
}

// --- fake dispatcher --------------------------------------------------------

// fakeDispatcher is a scripted httpapi.Dispatcher.
//
// PlanFn defaults to a plan that mirrors what forge.Client.Plan produces on a
// COLD cache with tier 1 armed — the deployment default. Note what this fake
// does NOT offer: a way to hand the handler a forge.PlanOptions. The hazard
// exists because a zero-valued PlanOptions writes correlation
// `window` where tier 1 is on, and the defence is that the Dispatcher
// interface exposes Plan and not PlanDispatch, so no call site can construct
// one. Reproducing that call site here would test a hazard the design removed.
type fakeDispatcher struct {
	mu sync.Mutex

	PlanFn     func(forge.DispatchRequest) (forge.DispatchPlan, error)
	DispatchFn func(forge.DispatchRequest, forge.DispatchPlan) (*forge.DispatchResult, error)

	Plans     []forge.DispatchRequest
	Dispatchs []forge.DispatchRequest
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		PlanFn: func(req forge.DispatchRequest) (forge.DispatchPlan, error) {
			return forge.DispatchPlan{
				Ref:                 req.Ref,
				Inputs:              req.Inputs,
				Correlation:         forge.CorrelationDispatchResponse,
				InjectionSkipReason: forge.SkipCacheCold,
				InputsValidation:    forge.InputsValidationSkippedColdCache,
				ReturnRunDetails:    true,
			}, nil
		},
		DispatchFn: func(_ forge.DispatchRequest, plan forge.DispatchPlan) (*forge.DispatchResult, error) {
			return &forge.DispatchResult{
				Plan:                      plan,
				UpstreamStatus:            http.StatusOK,
				ReturnRunDetailsRequested: plan.ReturnRunDetails,
				RunID:                     4242,
				HTMLURL:                   "https://github.com/acme/core/actions/runs/4242",
				UpstreamDuration:          12 * time.Millisecond,
			}, nil
		},
	}
}

func (d *fakeDispatcher) Plan(_ context.Context, req forge.DispatchRequest) (forge.DispatchPlan, error) {
	d.mu.Lock()
	d.Plans = append(d.Plans, req)
	fn := d.PlanFn
	d.mu.Unlock()
	return fn(req)
}

func (d *fakeDispatcher) Dispatch(_ context.Context, req forge.DispatchRequest, plan forge.DispatchPlan) (*forge.DispatchResult, error) {
	d.mu.Lock()
	d.Dispatchs = append(d.Dispatchs, req)
	fn := d.DispatchFn
	d.mu.Unlock()
	return fn(req, plan)
}

func (d *fakeDispatcher) RateSnapshots() []forge.RateSnapshot { return nil }

func (d *fakeDispatcher) DispatchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.Dispatchs)
}

// --- harness ----------------------------------------------------------------

type harness struct {
	t      *testing.T
	srv    *Server
	http   *httptest.Server
	store  *fakeStore
	forge  *fakeDispatcher
	reg    *metrics.Registry
	clock  *testutil.Clock
	cfg    *config.Config
	token  string
	events *testutil.IDSeq
}

// newHarness builds a Server over the fakes and serves its public handler.
func newHarness(t *testing.T, o configOpts) *harness {
	t.Helper()
	token, hash, err := MintToken(o.tokenID)
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	o.tokenHash = hash
	cfg := buildConfig(t, o)

	clock := testutil.NewClock(testutil.BaseTime)
	ids := testutil.NewIDSeq("evt_")
	reqIDs := testutil.NewIDSeq("req_")
	st := newFakeStore()
	fd := newFakeDispatcher()
	reg := metrics.New()

	srv, err := New(Options{
		Reloader:     config.NewReloader(cfg),
		Store:        st,
		Forge:        fd,
		Metrics:      reg,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:      "test",
		Now:          clock.Now,
		NewEventID:   ids.Next,
		NewRequestID: reqIDs.Next,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	ts := httptest.NewServer(srv.PublicHandler())
	t.Cleanup(ts.Close)

	return &harness{
		t: t, srv: srv, http: ts, store: st, forge: fd,
		reg: reg, clock: clock, cfg: cfg, token: token, events: ids,
	}
}

func newDefaultHarness(t *testing.T) *harness { return newHarness(t, defaultConfigOpts()) }

type triggerReq struct {
	method      string
	path        string
	body        string
	contentType string
	auth        string
	idemKey     string
	headers     map[string]string
	noAuth      bool
}

func (h *harness) do(r triggerReq) *http.Response {
	h.t.Helper()
	method := r.method
	if method == "" {
		method = http.MethodPost
	}
	path := r.path
	if path == "" {
		path = "/t/acme/core/release.yml"
	}
	body := r.body
	if body == "" {
		body = `{"ref":"refs/heads/main","inputs":{},"reason":"smoke test"}`
	}
	req, err := http.NewRequest(method, h.http.URL+path, bytes.NewReader([]byte(body)))
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	ct := r.contentType
	if ct == "" {
		ct = "application/json"
	}
	if ct != "-" {
		req.Header.Set("Content-Type", ct)
	}
	if !r.noAuth {
		auth := r.auth
		if auth == "" {
			auth = "Bearer " + h.token
		}
		req.Header.Set("Authorization", auth)
	}
	key := r.idemKey
	if key == "" {
		key = "idem-" + fmt.Sprint(time.Now().UnixNano())
	}
	if key != "-" {
		req.Header.Set("Idempotency-Key", key)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	resp, err := h.http.Client().Do(req)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	return resp
}

// decode reads a JSON response body and closes it.
func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	var out map[string]any
	if len(bytes.TrimSpace(raw)) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}
	return out
}

// counterValue reads one counter series out of the registry's Prometheus text.
func (h *harness) counterValue(name string, labels map[string]string) float64 {
	h.t.Helper()
	var buf bytes.Buffer
	if err := h.reg.Write(&buf); err != nil {
		h.t.Fatalf("metrics write: %v", err)
	}
	return findMetric(h.t, buf.String(), name, labels)
}
