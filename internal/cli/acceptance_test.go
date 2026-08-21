package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/adapter"
	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/httpapi"
	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// THE PHASE 0 ACCEPTANCE PROCEDURE, EXECUTED.
//
// The acceptance procedure is what answers "is Phase 0 done". Until this file existed it
// was a list of shell commands nobody had run: the gateway compiled, the store
// passed 56 tests, and no request had ever travelled trigger -> forwarded ->
// correlated -> traced -> exported against anything resembling GitHub.
//
// What this runs, in the procedure's order and with its expected results:
//
//	1. standpipe status                      exit 0, no cold cache, quota fine
//	2. POST /t/acme/core/test.yml          200, idempotency "new", run_id present
//	3. standpipe trace <id>                  TRIGGER / OUTCOME / RUN LINK / COVERAGE,
//	                                       resolution exact, source dispatch_response,
//	                                       and the content_sha256 integrity check
//	4. standpipe inventory                   the workflow reports coverage "covered"
//	5. replay the same Idempotency-Key     idempotency "replay", the SAME run_id
//	6. POST an unlisted workflow           403, and a trigger_rejected event exists
//	7. standpipe export                      every event comes out as canonical JSON
//	                                       and every content_sha256 verifies
//
// WHAT IT IS NOT: a real caller against real GitHub. We hold no credentials.
// It is the whole path with GitHub replaced by internal/testutil's fake, which
// answers 204-without-a-body by default and 200-with-a-run-id only when the
// request asks for run details. That distinction is the one thing that would
// make this procedure lie if the fake got it wrong.
//
// ONE DEVIATION FROM THE PROCEDURE, STATED RATHER THAN HIDDEN: it drives a gateway
// started with `standpiped --config ...`. That binary cannot be pointed at
// a fake GitHub — forge.Config has a BaseURL field, but neither the YAML nor
// internal/cli/gateway.go carries a key for it, so the compiled daemon can only
// ever talk to api.github.com. So the composition below rebuilds exactly what
// runGateway builds (same store, same adapter, same forge.Client, same
// httpapi.Server, real TCP listeners) with that one field set. Every CLI step
// is the real command function, reading the same standpipe.yaml from disk through
// its own second database handle, exactly as an operator's shell would.
// ---------------------------------------------------------------------------

const acceptanceWorkflowYAML = `
name: test
run-name: test ${{ inputs.standby_trigger_id }}
on:
  workflow_dispatch:
    inputs:
      standby_trigger_id:
        description: correlation id injected by the gateway
        required: false
      version:
        required: false
jobs:
  noop:
    runs-on: ubuntu-latest
    steps:
      - run: "true"
`

// notAllowedWorkflowYAML exists on GitHub and is NOT in allowed_targets. Step
// 6 needs a target that is real upstream and refused here, or the 403
// proves only that the workflow does not exist.
const notAllowedWorkflowYAML = `
name: not-allowed
on:
  workflow_dispatch: {}
`

type acceptance struct {
	t   *testing.T
	dir string

	cfgPath string
	cfg     *config.Config
	gh      *testutil.GitHub
	db      *store.DB
	client  *forge.Client
	srv     *httpapi.Server
	public  *httptest.Server
	admin   *httptest.Server
	bg      *background
	token   string
	logs    *bytes.Buffer
}

func newAcceptance(t *testing.T) *acceptance {
	t.Helper()
	dir := t.TempDir()

	gh := testutil.NewGitHub()
	t.Cleanup(gh.Close)
	gh.AddRepo("acme/core", "main", 5001)
	gh.AddWorkflow("acme/core", "test.yml", acceptanceWorkflowYAML)
	gh.AddWorkflow("acme/core", "not-allowed.yml", notAllowedWorkflowYAML)

	keyPath, err := testutil.WriteAppKey(dir)
	if err != nil {
		t.Fatalf("WriteAppKey: %v", err)
	}
	token, hash, err := httpapi.MintToken("tok_a1")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}

	dbPath := filepath.Join(dir, "gateway.db")
	cfgPath := filepath.Join(dir, "standpipe.yaml")
	cfgYAML := fmt.Sprintf(`
gateway:
  listen: "127.0.0.1:0"
  admin_listen: "127.0.0.1:1"
  idempotency_ttl: "24h"
  max_body_bytes: 65536
  upstream_timeout: "5s"
  shutdown_grace: "10s"
  retention_days: 90
  rate_limit:
    per_caller: {rate_per_second: 100, burst: 100}
    global: {rate_per_second: 100, burst: 100}
    pre_auth_ip: {rate_per_second: 100, burst: 100}
    reject_log_window: "1s"
storage:
  sqlite_path: %q
github:
  app_id: 12345
  private_key_path: %q
  max_inflight: 4
  max_queue_wait: "2s"
allowed_targets:
  - repo: "acme/core"
    workflows: ["test.yml"]
    refs: ["refs/heads/main"]
callers:
  release-bot:
    token_hash: %q
    token_id: "tok_a1"
    role: "admin"
    allow:
      # The caller's own policy is WIDER than the server-side allowlist on the
      # ref axis, deliberately. The server-side allowlist is the second layer and always
      # wins, and this is the only arrangement in which its refusal
      # (not_in_allowlist) is reachable at all — see step 6b.
      - repo: "acme/core"
        workflow: "test.yml"
        ref: "*"
`, dbPath, keyPath, hash)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing standpipe.yaml: %v", err)
	}

	// Startup's precondition: the operator runs `standpipe migrate` first.
	// Using the real command rather than store.Migrate is deliberate — the
	// gateway refuses to start on a pending migration, so this is part of the
	// procedure and not setup.
	if out, err := capture(t, func() error { return runMigrate([]string{"--config", cfgPath}) }); err != nil {
		t.Fatalf("standpipe migrate: %v\n%s", err, out)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := store.Open(context.Background(), store.Config{
		Path:         cfg.Storage.SQLitePath,
		MinFreeBytes: cfg.Storage.MinFreeBytes,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	reg := metrics.New()
	reloader := config.NewReloader(cfg)
	fstore := &adapter.ForgeStore{DB: db}

	client, err := forge.New(forge.Config{
		AppID:                    cfg.GitHub.AppID,
		PrivateKeyPath:           cfg.GitHub.PrivateKeyPath,
		MaxInflight:              cfg.GitHub.MaxInflight,
		MaxQueueWait:             cfg.GitHub.MaxQueueWait.D(),
		UpstreamTimeout:          cfg.Gateway.UpstreamTimeout.D(),
		ReturnRunDetails:         cfg.GitHub.ReturnRunDetails,
		InventoryHistoryDays:     cfg.Inventory.HistoryDays,
		InventoryRefreshInterval: cfg.Inventory.RefreshInterval.D(),
		// THE ONE FIELD runGateway DOES NOT SET. See the note at the top.
		BaseURL:   gh.URL(),
		UserAgent: "standpipe/acceptance",
	}, forge.WithStore(fstore), forge.WithMetrics(reg))
	if err != nil {
		t.Fatalf("forge.New: %v", err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Reloader: reloader, Store: db, Forge: client,
		Metrics: reg, Log: log, Version: version,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	public := httptest.NewServer(srv.PublicHandler())
	t.Cleanup(public.Close)
	admin := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(admin.Close)

	return &acceptance{
		t: t, dir: dir, cfgPath: cfgPath, cfg: cfg, gh: gh, db: db,
		client: client, srv: srv, public: public, admin: admin, token: token, logs: logs,
		bg: &background{log: log, db: db, gh: client, srv: srv, reg: reg, reloader: reloader, cfg: cfg},
	}
}

// bootstrap runs the startup inventory pass: surface discovery and run
// history. The procedure's preconditions assume it has happened — the coverage
// query joins run_link against github_runs, and without a history pass every
// trigger reads `unknown` no matter how well correlation worked.
func (a *acceptance) bootstrap() {
	a.t.Helper()
	a.bg.runInventory(context.Background(), "bootstrap")
}

// trigger posts one trigger request body, exactly as step 2's curl does.
func (a *acceptance) trigger(workflow, idemKey, body string) (*http.Response, map[string]any) {
	a.t.Helper()
	req, err := http.NewRequest(http.MethodPost, a.public.URL+"/t/acme/core/"+workflow, strings.NewReader(body))
	if err != nil {
		a.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Idempotency-Key", idemKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.public.Client().Do(req)
	if err != nil {
		a.t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatalf("reading body: %v", err)
	}
	var decoded map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		a.t.Fatalf("decoding %q: %v", raw, err)
	}
	return resp, decoded
}

// capture runs a CLI command with os.Stdout redirected, returning what it
// printed. The commands write to os.Stdout directly (trace and status both
// specify the screen), so this is how the procedure reads their output.
func capture(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w

	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&buf, r)
	}()

	runErr := fn()

	os.Stdout = orig
	_ = w.Close()
	wg.Wait()
	_ = r.Close()
	return buf.String(), runErr
}

// ---------------------------------------------------------------------------
// The procedure.
// ---------------------------------------------------------------------------

func TestAcceptanceProcedure(t *testing.T) {
	a := newAcceptance(t)
	a.bootstrap()

	// ---- STEP 1: is the system ready? -------------------------------------
	//
	// Expected: "exit code 0; no cold cache, quota sufficient, installation ok".
	var status statusReport
	t.Run("step 1 — standpipe status", func(t *testing.T) {
		out, err := capture(t, func() error {
			return runStatusCmd([]string{"--config", a.cfgPath, "--admin-url", a.admin.URL, "--json"})
		})
		if err != nil {
			t.Fatalf("standpipe status exited non-zero: %v\n%s", err, out)
		}
		if err := json.Unmarshal([]byte(out), &status); err != nil {
			t.Fatalf("status --json is not JSON: %v\n%s", err, out)
		}
		if !status.Gateway.Reachable {
			t.Errorf("status reports the gateway unreachable: %s", status.Gateway.Unreachable)
		}
		if !status.Gateway.SQLiteOK {
			t.Error("status reports sqlite_ok false")
		}
		if status.WorkflowCache.Cold != 0 {
			t.Errorf("step 1 expects NO cold workflow cache, got %d cold: %+v",
				status.WorkflowCache.Cold, status.WorkflowCache)
		}
		if len(status.Problems) != 0 {
			t.Errorf("status reported problems: %v", status.Problems)
		}
	})

	// ---- STEP 2: trigger ---------------------------------------------------
	//
	// Expected: 200 OK, "idempotency": "new", a trigger_event_id, and run_id ALREADY
	// IN THE RESPONSE (tier 1). Its absence is not a failure, but step 3 makes
	// it visible — so this step asserts what a tier-1-armed deployment must do.
	const idemKey = "smoke-0001"
	var eventID string
	var runID json.Number

	t.Run("step 2 — trigger", func(t *testing.T) {
		resp, body := a.trigger("test.yml", idemKey,
			`{"ref":"refs/heads/main","inputs":{},"reason":"smoke test"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
		}
		if got, _ := body["idempotency"].(string); got != string(store.IdempotencyNew) {
			t.Errorf("idempotency = %q, want %q", got, store.IdempotencyNew)
		}
		if got, _ := body["status"].(string); got != string(store.OutcomeForwarded) {
			t.Errorf("status = %q, want %q", got, store.OutcomeForwarded)
		}
		eventID, _ = body["trigger_event_id"].(string)
		if eventID == "" {
			t.Fatal("no trigger_event_id in the response; the rest of the procedure has nothing to trace")
		}
		var ok bool
		runID, ok = body["run_id"].(json.Number)
		if !ok || runID == "0" {
			t.Fatalf("TIER 1 DID NOT RUN: the response carried no run_id. Step 2 says this is not a "+
				"failure by itself, but it means the dispatch response did not include the created run "+
				"(tier 1). Body: %v", body)
		}
		if url, _ := body["run_html_url"].(string); !strings.HasPrefix(url, "https://github.com/acme/core/actions/runs/") {
			t.Errorf("run_html_url = %q, want the BROWSER url (GitHub returns two; the gateway returns html_url)", url)
		}
		// The armament, which is a different fact from the tier that fired
		// test.yml declares standby_trigger_id, so injection
		// happened and the label is `injected` even though tier 1 also ran.
		if got, _ := body["correlation"].(string); got != string(store.CorrelationInjected) {
			t.Errorf("correlation = %q, want %q", got, store.CorrelationInjected)
		}

		// What actually went on the wire.
		sent := a.gh.Dispatches()
		if len(sent) != 1 {
			t.Fatalf("want exactly one dispatch on the wire, got %d", len(sent))
		}
		if !sent[0].ReturnRunDetails {
			t.Error("return_run_details was not sent, yet the response carried a run id")
		}
		if sent[0].StatusReturned != http.StatusOK {
			t.Errorf("the fake answered %d; with tier 1 armed GitHub answers 200, not 204",
				sent[0].StatusReturned)
		}
		if got, _ := sent[0].Inputs[forge.ReservedInputKey].(string); got != eventID {
			t.Errorf("standby_trigger_id on the wire = %q, want the trigger event id %q", got, eventID)
		}
	})
	if t.Failed() {
		t.Fatal("the procedure cannot continue past step 2")
	}

	// ---- STEP 3: read the whole life of the trigger ------------------------
	//
	// Expected: one screen carrying TRIGGER (with `integrity content_sha256 OK`),
	// OUTCOME (`status forwarded`, `http 200`), RUN LINK (`resolution exact`,
	// `source dispatch_response (tier 1)`) and `COVERAGE covered`.
	t.Run("step 3 — standpipe trace", func(t *testing.T) {
		out, err := capture(t, func() error {
			return runTraceCmd([]string{"--config", a.cfgPath, eventID})
		})
		if err != nil {
			t.Fatalf("standpipe trace failed: %v\n%s", err, out)
		}
		for _, want := range []string{
			"TRIGGER", "OUTCOME", "RUN LINK", "COVERAGE",
			"status      forwarded",
			"http 200",
			"resolution  exact",
			"dispatch_response  (tier 1)",
			"correlation injected",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("step 3 expects %q in the trace; it is not there.\n---\n%s", want, out)
			}
		}
		if !strings.Contains(out, "COVERAGE  "+store.CoverageCovered) {
			t.Errorf("step 3 expects COVERAGE covered.\n---\n%s", out)
		}

		// --- the integrity half of step 3.
		//
		// "content_sha256 verification is part of `trace`; there is no
		// separate integrity step." internal/store now exports
		// VerifyContentHash, so the recomputation exists; what is checked here
		// is BOTH that the event verifies AND that `trace` says so, because a
		// trace that prints NOT VERIFIED leaves step 3's central claim
		// unproven no matter how healthy the row is.
		res, verr := a.db.VerifyContentHash(context.Background(), eventID)
		if verr != nil {
			t.Fatalf("VerifyContentHash: %v", verr)
		}
		if !res.Verdict.Verified() {
			t.Errorf("the trigger event does not verify: verdict=%s reason=%s detail=%s",
				res.Verdict, res.Reason, res.Detail)
		}
		if strings.Contains(out, "NOT VERIFIED") {
			t.Errorf("`standpipe trace` prints `content_sha256 NOT VERIFIED` while "+
				"store.VerifyContentHash returns %s for the same event. Step 3's integrity check is "+
				"the operator's evidence that the log is intact, and it is not wired up: "+
				"internal/cli/trace_cmd.go pins traceResult.Integrity to integrityUnavailable.\n---\n%s",
				res.Verdict, out)
		}
	})

	// ---- STEP 4: inventory -------------------------------------------------
	//
	// Expected: `standpipe inventory | jq '.observed[] | select(.workflow=="test.yml")'`
	// reports coverage "covered".
	t.Run("step 4 — standpipe inventory", func(t *testing.T) {
		// AN IMPLICIT PRECONDITION THE PROCEDURE DOES NOT STATE. The coverage query is driven
		// FROM github_runs: a trigger counts as `covered` only once the run it
		// links to is in the run cache. The cache is filled by the inventory
		// pass, which runs at startup and then every
		// inventory.refresh_interval — 6 HOURS by default. An operator
		// following the procedure literally, triggering seconds after starting the
		// gateway, sees `unknown` for a trigger that correlated perfectly, and
		// `standpipe trace` says `covered` for the very same trigger because it
		// answers from the run_link alone.
		//
		// The refresh is run here so step 4 tests
		// coverage rather than cache latency.
		a.bootstrap()

		out, err := capture(t, func() error {
			return runInventoryCmd([]string{"--config", a.cfgPath})
		})
		if err != nil {
			t.Fatalf("standpipe inventory failed: %v\n%s", err, out)
		}
		var report httpapi.InventoryReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("inventory output is not JSON: %v\n%s", err, out)
		}
		if len(report.Surface) == 0 {
			t.Error("the bootstrapped surface is empty; Phase 0's exit condition requires inventory to show the list")
		}
		var found *httpapi.ObservedEntry
		for i := range report.Observed {
			if report.Observed[i].Workflow == "test.yml" {
				found = &report.Observed[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("no observed row for test.yml. Step 4's jq filter would print nothing.\n%s", out)
		}
		if found.Coverage != store.CoverageCovered {
			t.Errorf("coverage = %q, want %q — the trigger went through the gateway and its run id is "+
				"settled, so this is the case the coverage query calls covered", found.Coverage, store.CoverageCovered)
		}
		if report.CoveredCount < 1 {
			t.Errorf("covered count = %d, want at least 1", report.CoveredCount)
		}
	})

	// ---- STEP 5: replay ----------------------------------------------------
	//
	// Expected: the same Idempotency-Key answers "replay" and the SAME run_id.
	t.Run("step 5 — replay", func(t *testing.T) {
		before := len(a.gh.Dispatches())
		resp, body := a.trigger("test.yml", idemKey,
			`{"ref":"refs/heads/main","inputs":{},"reason":"smoke test"}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
		}
		if got, _ := body["idempotency"].(string); got != string(store.IdempotencyReplay) {
			t.Errorf("idempotency = %q, want %q", got, store.IdempotencyReplay)
		}
		if got, _ := body["run_id"].(json.Number); got.String() != runID.String() {
			t.Errorf("run_id = %s, want the ORIGINAL's %s (a replay returns the cached response)", got, runID)
		}
		if after := len(a.gh.Dispatches()); after != before {
			t.Errorf("a replay dispatched to GitHub: %d -> %d. A replay must never dispatch, or the workflow would "+
				"have run twice", before, after)
		}
	})

	// ---- STEP 6: the rejection log ----------------------------------------
	//
	// The procedure calls this the testable form of "silent dropping is
	// FORBIDDEN": a refusal must leave a record, not just a status code.
	t.Run("step 6a — the procedure's own example: an unlisted workflow is refused AND recorded", func(t *testing.T) {
		resp, body := a.trigger("not-allowed.yml", "smoke-reject-0001",
			`{"ref":"refs/heads/main"}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", resp.StatusCode, body)
		}

		// The rejection path writes this event inline (only coalesced 429s are deferred),
		// so it is on disk by the time the response arrives.
		rejectedID := a.findRejectedEvent(t, "not-allowed.yml")
		out, err := capture(t, func() error {
			return runTraceCmd([]string{"--config", a.cfgPath, rejectedID})
		})
		if err != nil {
			t.Fatalf("standpipe trace on the rejection failed: %v\n%s", err, out)
		}
		for _, want := range []string{"REJECTED", "http 403"} {
			if !strings.Contains(out, want) {
				t.Errorf("step 6 expects %q in the trace of the rejection.\n---\n%s", want, out)
			}
		}

		// SPEC NOTE, reported rather than asserted as a failure. Step 6
		// expects `reject_reason: not_in_allowlist` from exactly this request.
		// It cannot happen: the config cross-check
		// (internal/config/validate.go, "is not in allowed_targets, so this
		// entry can never authorize anything") REFUSES TO LOAD a caller whose
		// allow list names a workflow the server-side allowlist does not
		// carry. So a workflow outside allowed_targets is necessarily outside
		// the caller's policy too, and the caller-policy check refuses it first, with
		// reason caller_policy. Step 6b below reaches not_in_allowlist by the
		// only route that exists.
		detail, _ := body["detail"].(string)
		if detail != httpapi.DetailCallerPolicy && detail != httpapi.DetailNotInAllowlist {
			t.Errorf("detail = %q, want one of %q / %q", detail, httpapi.DetailCallerPolicy, httpapi.DetailNotInAllowlist)
		}
		if detail == httpapi.DetailCallerPolicy {
			t.Logf("step 6 divergence (reported, not a code bug): the procedure expects "+
				"reject_reason not_in_allowlist for POST /t/acme/core/not-allowed.yml, and the gateway "+
				"answers %q. The config cross-check makes the procedure's stated reason unreachable for its "+
				"own example — a caller allow entry outside allowed_targets fails validation, so the caller-policy "+
				"check always fires first.", store.RejectCallerPolicy)
		}
	})

	t.Run("step 6b — not_in_allowlist is reachable, by the one route that exists", func(t *testing.T) {
		// The caller's policy allows every ref on test.yml; allowed_targets
		// allows only refs/heads/main. The server-side allowlist is what refuses.
		resp, body := a.trigger("test.yml", "smoke-reject-0002",
			`{"ref":"refs/heads/feature"}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", resp.StatusCode, body)
		}
		if got, _ := body["detail"].(string); got != httpapi.DetailNotInAllowlist {
			t.Fatalf("detail = %q, want %q", got, httpapi.DetailNotInAllowlist)
		}
		found := false
		if err := scanEvents(context.Background(), a.db, time.Time{}, func(ev eventObj) error {
			if ev.eventType() == string(store.EventTriggerRejected) &&
				ev.str("reject_reason") == string(store.RejectNotInAllowlist) {
				found = true
			}
			return nil
		}); err != nil {
			t.Fatalf("scanning the event log: %v", err)
		}
		if !found {
			t.Error("silent-drop rule VIOLATED: a not_in_allowlist refusal left no trigger_rejected event")
		}
	})

	// ---- STEP 7: the archive ----------------------------------------------
	//
	// Expected: every event above comes out as line-delimited canonical JSON. And
	// the claim the log rests on — that a line can be verified — is checked
	// here for EVERY event, not just the one step 3 traced.
	t.Run("step 7 — standpipe export, and every content_sha256 verifies", func(t *testing.T) {
		outPath := filepath.Join(a.dir, "events.jsonl")
		if _, err := capture(t, func() error {
			return runExportCmd([]string{"--config", a.cfgPath, "--out", outPath})
		}); err != nil {
			t.Fatalf("standpipe export failed: %v", err)
		}
		raw, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("reading the export: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Fatal("the export is empty")
		}

		seen := map[string]int{}
		for i, line := range lines {
			var ev map[string]any
			dec := json.NewDecoder(strings.NewReader(line))
			dec.UseNumber()
			if err := dec.Decode(&ev); err != nil {
				t.Fatalf("line %d is not JSON: %v\n%s", i+1, err, line)
			}
			typ, _ := ev["event_type"].(string)
			id, _ := ev["event_id"].(string)
			seen[typ]++
			if id == "" {
				t.Errorf("line %d has no event_id", i+1)
				continue
			}
			if sha, _ := ev["content_sha256"].(string); sha == "" {
				t.Errorf("%s %s exported without a content_sha256", typ, id)
			}
			res, err := a.db.VerifyContentHash(context.Background(), id)
			if err != nil {
				t.Fatalf("VerifyContentHash(%s): %v", id, err)
			}
			if !res.Verdict.Verified() {
				t.Errorf("%s %s does not verify: verdict=%s reason=%s detail=%s",
					typ, id, res.Verdict, res.Reason, res.Detail)
			}
		}

		// The procedure names the five types. run_observed only appears once a run has a
		// conclusion, which this procedure does not wait for, so it is
		// reported rather than required.
		for _, typ := range []string{
			string(store.EventTrigger), string(store.EventTriggerOutcome),
			string(store.EventTriggerRejected), string(store.EventRunLink),
		} {
			if seen[typ] == 0 {
				t.Errorf("step 7 expects %s events in the export; there are none. Saw: %v", typ, seen)
			}
		}
		t.Logf("export: %d line(s), %v", len(lines), seen)
	})
}

// findRejectedEvent scans the event log for the trigger_rejected of a workflow.
func (a *acceptance) findRejectedEvent(t *testing.T, workflow string) string {
	t.Helper()
	var id string
	err := scanEvents(context.Background(), a.db, time.Time{}, func(ev eventObj) error {
		if ev.eventType() == string(store.EventTriggerRejected) && ev.str("target", "workflow") == workflow {
			id = ev.eventID()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the event log: %v", err)
	}
	if id == "" {
		t.Fatalf("silent-drop rule VIOLATED: the request was refused with a 403 and NO trigger_rejected event was "+
			"written for %s. A refusal that leaves no record is the silent dropping this project "+
			"puts first among failure modes", workflow)
	}
	return id
}

// ---------------------------------------------------------------------------
// The tier-2 half of acceptance step 3.
// ---------------------------------------------------------------------------

// TestAcceptanceTier2WithReturnRunDetailsOff runs step 3's optional
// paragraph: turn tier 1 OFF, dispatch into a workflow whose run-name carries
// standby_trigger_id, and expect `source: run_name (tier 2)` inside the
// resolver horizon.
//
// This is the outage case. Tier 2 is the only tier that survives a lost
// dispatch response, and until it is exercised the operator's YAML edit — the
// one thing the onboarding hardening report measures — has never been shown to do
// anything.
func TestAcceptanceTier2WithReturnRunDetailsOff(t *testing.T) {
	a := newAcceptance(t)

	// The escape hatch. The client is rebuilt because github.* is not
	// reloadable and a restart is what an operator would do.
	off := false
	client, err := forge.New(forge.Config{
		AppID:            a.cfg.GitHub.AppID,
		PrivateKeyPath:   a.cfg.GitHub.PrivateKeyPath,
		MaxInflight:      4,
		MaxQueueWait:     2 * time.Second,
		UpstreamTimeout:  5 * time.Second,
		ReturnRunDetails: &off,
		BaseURL:          a.gh.URL(),
	}, forge.WithStore(&adapter.ForgeStore{DB: a.db}))
	if err != nil {
		t.Fatalf("forge.New: %v", err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Reloader: config.NewReloader(a.cfg), Store: a.db, Forge: client,
		Metrics: metrics.New(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: version,
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	public := httptest.NewServer(srv.PublicHandler())
	t.Cleanup(public.Close)
	a.public = public

	a.bootstrap()

	resp, body := a.trigger("test.yml", "smoke-tier2-0001",
		`{"ref":"refs/heads/main","inputs":{},"reason":"tier 2"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", resp.StatusCode, body)
	}
	// Tier 1 is off, so GitHub answered 204 with no body and the caller gets
	// no run id. That is not a failure.
	if _, present := body["run_id"]; present {
		t.Errorf("run_id present with return_run_details off: %v", body["run_id"])
	}
	if got, _ := body["correlation"].(string); got != string(store.CorrelationInjected) {
		t.Errorf("correlation = %q, want %q — injection still happened", got, store.CorrelationInjected)
	}
	sent := a.gh.Dispatches()
	if last := sent[len(sent)-1]; last.ReturnRunDetails || last.StatusReturned != http.StatusNoContent {
		t.Errorf("with tier 1 off the dispatch must be a plain 204: %+v", last)
	}

	eventID, _ := body["trigger_event_id"].(string)

	// The background resolver's sweep, run once rather than waited for.
	stats, err := forge.NewResolver(client).Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Exact != 1 {
		t.Fatalf("sweep resolved %d trigger(s) exactly, want 1: %+v", stats.Exact, stats)
	}

	out, err := capture(t, func() error {
		return runTraceCmd([]string{"--config", a.cfgPath, eventID})
	})
	if err != nil {
		t.Fatalf("standpipe trace: %v\n%s", err, out)
	}
	if !strings.Contains(out, "resolution  exact") {
		t.Errorf("step 3's tier-2 paragraph expects resolution exact.\n---\n%s", out)
	}
	if !strings.Contains(out, "run_name  (tier 2") {
		t.Errorf("step 3's tier-2 paragraph expects `source: run_name (tier 2)`. This is the line "+
			"that answers \"did the operator's run-name edit do anything\" — without it the adoption "+
			"report has no source.\n---\n%s", out)
	}
	// http 204 recorded verbatim, and still `forwarded`.
	if !strings.Contains(out, "http 204") {
		t.Errorf("the outcome did not record GitHub's 204 verbatim.\n---\n%s", out)
	}
	if !strings.Contains(out, "status      forwarded") {
		t.Errorf("a 204 was not recorded as forwarded; the status code is not the success predicate.\n---\n%s", out)
	}
}
