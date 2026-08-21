package forge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/testutil"
)

// These tests are in `package forge` because two of the things they pin —
// correlationFor's ordering and matchWindowBounds' window arithmetic — are
// unexported and are exactly where a "simplification" would be silent.
//
// internal/testutil imports nothing of ours, so there is no cycle.

// --- a store the resolver can run against ----------------------------------

// memStore is an in-memory forge.Store. It is not a database: the resolver's
// state lives in the DB in production, and what these tests exercise is the
// decision, not the persistence.
type memStore struct {
	mu       sync.Mutex
	cache    map[string]WorkflowCacheEntry
	pending  []PendingTrigger
	links    []RunLink
	observed []RunObserved
}

func newMemStore() *memStore {
	return &memStore{cache: make(map[string]WorkflowCacheEntry)}
}

func cacheKey(repo, workflow, ref string) string { return repo + "|" + workflow + "|" + ref }

func (m *memStore) GetWorkflowCache(_ context.Context, repo, workflow, ref string) (WorkflowCacheEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.cache[cacheKey(repo, workflow, ref)]
	return e, ok, nil
}

func (m *memStore) PutWorkflowCache(_ context.Context, e WorkflowCacheEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache[cacheKey(e.Repo, e.Workflow, e.Ref)] = e
	return nil
}

func (m *memStore) UpsertRuns(_ context.Context, runs []RunRecord) ([]RunRecord, error) {
	return runs, nil
}

func (m *memStore) PendingCorrelations(_ context.Context, _ time.Duration, _ int) ([]PendingTrigger, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PendingTrigger, len(m.pending))
	copy(out, m.pending)
	return out, nil
}

func (m *memStore) WriteRunLink(_ context.Context, l RunLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.links = append(m.links, l)
	return nil
}

func (m *memStore) WriteRunObserved(_ context.Context, o RunObserved) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observed = append(m.observed, o)
	return nil
}

func (m *memStore) Links() []RunLink {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RunLink, len(m.links))
	copy(out, m.links)
	return out
}

// --- harness ----------------------------------------------------------------

const workflowYAML = `
name: release
on:
  workflow_dispatch:
    inputs:
      standby_trigger_id:
        description: correlation id injected by the gateway
        required: false
      version:
        required: false
run-name: release ${{ inputs.standby_trigger_id }}
`

// workflowYAMLNoTriggerInput is a workflow that has NOT been onboarded: it
// declares workflow_dispatch but no standby_trigger_id, so injection rule 1 forbids
// injection and tier 2 is unavailable.
const workflowYAMLNoTriggerInput = `
name: test
on:
  workflow_dispatch:
    inputs:
      version:
        required: false
`

// --- a metrics registry the tests can read back -----------------------------

// memMetrics records every counter and gauge the client emits. It exists
// because some degradations have NO other symptom: no degradation may be silent,
// so they must surface as a counter, and a counter nothing asserts on is the same
// as no counter at all.
type memMetrics struct {
	mu       sync.Mutex
	counters []metricSample
}

type metricSample struct {
	name   string
	labels map[string]string
}

func (m *memMetrics) IncCounter(name string, labels map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make(map[string]string, len(labels))
	for k, v := range labels {
		cp[k] = v
	}
	m.counters = append(m.counters, metricSample{name: name, labels: cp})
}

func (m *memMetrics) SetGauge(string, float64, map[string]string) {}

// samples returns every emission of one counter, oldest first.
func (m *memMetrics) samples(name string) []metricSample {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []metricSample
	for _, s := range m.counters {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

type env struct {
	t       *testing.T
	gh      *testutil.GitHub
	store   *memStore
	c       *Client
	clock   *testutil.Clock
	metrics *memMetrics
}

func newEnv(t *testing.T, mutate func(*Config)) *env {
	t.Helper()
	gh := testutil.NewGitHub()
	t.Cleanup(gh.Close)

	clock := testutil.NewClock(testutil.BaseTime)
	gh.SetNow(clock.Now)

	keyPath, err := testutil.WriteAppKey(t.TempDir())
	if err != nil {
		t.Fatalf("WriteAppKey: %v", err)
	}

	cfg := Config{
		AppID:          12345,
		PrivateKeyPath: keyPath,
		BaseURL:        gh.URL(),
		MaxInflight:    4,
	}
	if mutate != nil {
		mutate(&cfg)
	}

	st := newMemStore()
	mx := &memMetrics{}
	c, err := New(cfg, WithStore(st), WithMetrics(mx))
	if err != nil {
		t.Fatalf("forge.New: %v", err)
	}
	c.now = clock.Now

	return &env{t: t, gh: gh, store: st, c: c, clock: clock, metrics: mx}
}

// seed registers a repo and a workflow on the fake and warms the workflow
// cache the way DiscoverSurface would.
func (e *env) seed(repo, workflow, yaml string) int64 {
	e.t.Helper()
	e.gh.AddRepo(repo, "main", 5001)
	id := e.gh.AddWorkflow(repo, workflow, yaml)
	r, err := ParseRepo(repo)
	if err != nil {
		e.t.Fatalf("ParseRepo: %v", err)
	}
	if _, err := e.c.DiscoverSurface(context.Background(), r); err != nil {
		e.t.Fatalf("DiscoverSurface: %v", err)
	}
	return id
}

func boolPtr(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// The zero-valued PlanOptions hazard, tested through its OBSERVABLE CONSEQUENCE.
//
// The hazard is a zero-valued PlanOptions reading as "tier 1 off" and writing
// correlation `window` where tier 1 is on. The adapter's defence is that no
// call site constructs a PlanOptions: Client.Plan fills it from the resolved
// configuration. So the thing worth testing is not the struct — it is what
// correlation the CONFIGURATION produces, end to end.
// ---------------------------------------------------------------------------

func TestCorrelationFollowsTheResolvedConfiguration(t *testing.T) {
	cases := []struct {
		name             string
		returnRunDetails *bool
		workflow         string
		yaml             string
		want             Correlation
	}{
		{
			// The documented default is TRUE. An ABSENT key must mean
			// "default", never "off": reading nil as false is the zero-value hazard
			// and it writes a legal enum value with no external symptom.
			name:             "return_run_details ABSENT and no injection -> dispatch_response",
			returnRunDetails: nil,
			workflow:         "test.yml",
			yaml:             workflowYAMLNoTriggerInput,
			want:             CorrelationDispatchResponse,
		},
		{
			name:             "return_run_details TRUE and no injection -> dispatch_response",
			returnRunDetails: boolPtr(true),
			workflow:         "test.yml",
			yaml:             workflowYAMLNoTriggerInput,
			want:             CorrelationDispatchResponse,
		},
		{
			// `window` is reachable ONLY through the escape hatch.
			name:             "return_run_details FALSE and no injection -> window",
			returnRunDetails: boolPtr(false),
			workflow:         "test.yml",
			yaml:             workflowYAMLNoTriggerInput,
			want:             CorrelationWindow,
		},
		{
			// Ordering is by DURABILITY, not strength: an injected dispatch is
			// `injected` even though tier 1 was also armed and will answer
			// first. A strength-ordered variant makes `injected` unreachable
			// and deletes the operator checklist's hardening signal.
			name:             "injection wins over tier 1",
			returnRunDetails: boolPtr(true),
			workflow:         "release.yml",
			yaml:             workflowYAML,
			want:             CorrelationInjected,
		},
		{
			name:             "injection is still `injected` with tier 1 off",
			returnRunDetails: boolPtr(false),
			workflow:         "release.yml",
			yaml:             workflowYAML,
			want:             CorrelationInjected,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, func(c *Config) { c.ReturnRunDetails = tc.returnRunDetails })
			e.seed("acme/core", tc.workflow, tc.yaml)

			plan, err := e.c.Plan(context.Background(), DispatchRequest{
				Repo:           Repo{Owner: "acme", Name: "core"},
				Workflow:       tc.workflow,
				Ref:            "refs/heads/main",
				Inputs:         map[string]string{},
				TriggerEventID: "evt_0001",
			})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if plan.Correlation != tc.want {
				t.Errorf("correlation = %q, want %q (return_run_details=%v). "+
					"This value goes into a trigger row that is NEVER updated and sits inside the "+
					"content_sha256 hash scope; a wrong one is legal, permanent and symptomless.",
					plan.Correlation, tc.want, tc.returnRunDetails)
			}
			// Armament and the outgoing request must agree, or the record
			// describes a dispatch that did not happen.
			wantArmed := tc.returnRunDetails == nil || *tc.returnRunDetails
			if plan.ReturnRunDetails != wantArmed {
				t.Errorf("plan.ReturnRunDetails = %v, want %v", plan.ReturnRunDetails, wantArmed)
			}
			if injected := plan.Inputs[ReservedInputKey]; (injected != "") != (tc.want == CorrelationInjected) {
				t.Errorf("injection state disagrees with correlation: inputs[%s]=%q, correlation=%q",
					ReservedInputKey, injected, plan.Correlation)
			}
		})
	}
}

// TestArmsRunNameMatchIsExactlyInjected guards the negative-test rewrite the
// comment on ArmsRunNameMatch warns about. `c != CorrelationWindow` would
// promote tier-1-only triggers into the tier-2 branch, where the search cannot
// match and the trigger is held pending instead of being resolved from the
// window.
func TestArmsRunNameMatchIsExactlyInjected(t *testing.T) {
	for _, c := range []Correlation{CorrelationInjected, CorrelationDispatchResponse, CorrelationWindow, ""} {
		want := c == CorrelationInjected
		if got := c.ArmsRunNameMatch(); got != want {
			t.Errorf("Correlation(%q).ArmsRunNameMatch() = %v, want %v", c, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The wire, against a fake that behaves like GitHub.
// ---------------------------------------------------------------------------

// TestDispatchWireShape pins the dispatch mapping and the rule that a numeric
// HTTP status is never a success predicate, together: tier 1 armed
// answers 200 with a run id, tier 1 off answers 204 with NO BODY, and both are
// `forwarded`.
func TestDispatchWireShape(t *testing.T) {
	t.Run("tier 1 armed", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.ReturnRunDetails = boolPtr(true) })
		e.seed("acme/core", "release.yml", workflowYAML)

		req := DispatchRequest{
			Repo: Repo{Owner: "acme", Name: "core"}, Workflow: "release.yml",
			Ref: "refs/heads/main", Inputs: map[string]string{}, TriggerEventID: "evt_0001",
		}
		plan, err := e.c.Plan(context.Background(), req)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		res, err := e.c.Dispatch(context.Background(), req, plan)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if res.UpstreamStatus != 200 {
			t.Errorf("upstream status = %d, want 200 — with tier 1 armed a SUCCESS is not a 204",
				res.UpstreamStatus)
		}
		if res.RunID == 0 {
			t.Error("tier 1 was armed and the dispatch response carried no run id")
		}
		if res.HTMLURL == "" {
			t.Error("the dispatch response's html_url is missing; the trigger response returns the browser url as run_html_url")
		}

		sent := e.gh.Dispatches()
		if len(sent) != 1 {
			t.Fatalf("want one dispatch on the wire, got %d", len(sent))
		}
		if !sent[0].ReturnRunDetails {
			t.Error("return_run_details was not on the wire, yet the plan said tier 1 was armed")
		}
		if got := sent[0].Inputs[ReservedInputKey]; got != "evt_0001" {
			t.Errorf("standby_trigger_id on the wire = %q, want evt_0001", got)
		}
		// RunLinkFromDispatch is the only builder of a tier-1 link, so
		// `dispatch_response` is never spelled at a call site.
		link, ok := RunLinkFromDispatch("evt_0001", res, testutil.BaseTime)
		if !ok {
			t.Fatal("RunLinkFromDispatch refused a result carrying a run id")
		}
		if link.Resolution != ResolutionExact || link.ResolutionSource != ResolutionSourceDispatchResponse {
			t.Errorf("tier-1 link = %+v, want exact/dispatch_response", link)
		}
	})

	t.Run("tier 1 off", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.ReturnRunDetails = boolPtr(false) })
		e.seed("acme/core", "test.yml", workflowYAMLNoTriggerInput)

		req := DispatchRequest{
			Repo: Repo{Owner: "acme", Name: "core"}, Workflow: "test.yml",
			Ref: "refs/heads/main", Inputs: map[string]string{}, TriggerEventID: "evt_0002",
		}
		plan, err := e.c.Plan(context.Background(), req)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		res, err := e.c.Dispatch(context.Background(), req, plan)
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if res.UpstreamStatus != 204 {
			t.Errorf("upstream status = %d, want 204", res.UpstreamStatus)
		}
		if res.RunID != 0 {
			t.Errorf("run id = %d, want 0 — GitHub returns NO BODY on a 204", res.RunID)
		}
		if _, ok := RunLinkFromDispatch("evt_0002", res, testutil.BaseTime); ok {
			t.Error("a tier-1 run_link was built from a response that carried no run id")
		}
		sent := e.gh.Dispatches()
		if len(sent) != 1 || sent[0].ReturnRunDetails {
			t.Errorf("return_run_details was requested with tier 1 off: %+v", sent)
		}
	})
}

// TestUpstreamFailureShapesMapToDistinctKinds runs each failure the error
// mapping claims to distinguish through the REAL wire encoding.
//
// The secondary rate limit is the one worth the trouble: it arrives as a 403
// with a documentation_url suffix, not a 429, and a fake that sent 429 would
// exercise the primary-limit branch instead.
func TestUpstreamFailureShapesMapToDistinctKinds(t *testing.T) {
	cases := []struct {
		mode          testutil.FailMode
		want          Kind
		wantCacheable bool
		wantUnknown   bool
	}{
		{testutil.FailNotFound, KindWorkflowNotFound, true, false},
		{testutil.FailUnprocessable, KindUpstreamRejected, true, false},
		{testutil.FailSecondaryRateLimit, KindSecondaryRateLimit, false, false},
		{testutil.FailPrimaryRateLimit, KindPrimaryRateLimit, false, false},
		{testutil.FailServerError, KindUpstreamServerError, false, true},
		{testutil.FailHang, KindUpstreamTimeout, false, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode), func(t *testing.T) {
			e := newEnv(t, func(c *Config) {
				// Short enough that FailHang is a fast test and long enough
				// that the other cases are never mistaken for a timeout.
				c.UpstreamTimeout = 300 * time.Millisecond
			})
			e.seed("acme/core", "release.yml", workflowYAML)
			e.gh.Fail(testutil.OpDispatch, tc.mode)

			req := DispatchRequest{
				Repo: Repo{Owner: "acme", Name: "core"}, Workflow: "release.yml",
				Ref: "refs/heads/main", Inputs: map[string]string{}, TriggerEventID: "evt_0003",
			}
			plan, err := e.c.Plan(context.Background(), req)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			_, err = e.c.Dispatch(context.Background(), req, plan)
			if err == nil {
				t.Fatalf("%s: dispatch succeeded against an injected failure", tc.mode)
			}
			if got := KindOf(err); got != tc.want {
				t.Errorf("%s: Kind = %q, want %q (err %v)", tc.mode, got, tc.want, err)
			}
			var fe *Error
			if !errors.As(err, &fe) {
				t.Fatalf("%s: error is not a *forge.Error: %v", tc.mode, err)
			}
			if fe.Cacheable() != tc.wantCacheable {
				t.Errorf("%s: Cacheable() = %v, want %v — this decides whether the idempotency "+
					"record completes or stays retryable", tc.mode, fe.Cacheable(), tc.wantCacheable)
			}
			if fe.LeavesUnknown() != tc.wantUnknown {
				t.Errorf("%s: LeavesUnknown() = %v, want %v", tc.mode, fe.LeavesUnknown(), tc.wantUnknown)
			}
		})
	}
}

// TestDispatchIsNeverRetried pins the no-retry rule. A timeout on POST .../dispatches is
// ambiguous: the dispatch may already have landed. Exactly ONE POST must reach
// GitHub, or a production workflow runs twice.
func TestDispatchIsNeverRetried(t *testing.T) {
	e := newEnv(t, func(c *Config) { c.UpstreamTimeout = 200 * time.Millisecond })
	e.seed("acme/core", "release.yml", workflowYAML)
	e.gh.Fail(testutil.OpDispatch, testutil.FailHang)

	req := DispatchRequest{
		Repo: Repo{Owner: "acme", Name: "core"}, Workflow: "release.yml",
		Ref: "refs/heads/main", Inputs: map[string]string{}, TriggerEventID: "evt_0004",
	}
	plan, err := e.c.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := e.c.Dispatch(context.Background(), req, plan); KindOf(err) != KindUpstreamTimeout {
		t.Fatalf("Kind = %q, want upstream_timeout", KindOf(err))
	}
	if n := e.gh.Calls(testutil.OpDispatch); n != 1 {
		t.Errorf("no-retry rule VIOLATED: %d POSTs reached .../dispatches for one call. A dispatch timeout "+
			"is ambiguous and must never be retried automatically — the correlation resolver settles it", n)
	}
}
