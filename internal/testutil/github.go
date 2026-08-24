package testutil

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hangMax bounds FailHang so a fake can never outlive the test that built it.
// It is far above any upstream_timeout a test would configure.
const hangMax = 30 * time.Second

// ---------------------------------------------------------------------------
// A fake GitHub REST API.
//
// THE RULE THIS FILE IS WRITTEN UNDER: it mimics GitHub's REAL behaviour, never
// the convenient version. Four consequences, each of which a "helpful" fake
// would get wrong and each of which hides a real defect when it does:
//
//  1. POST .../dispatches answers 204 WITH NO BODY unless the request body
//     carries return_run_details: true, in which case it answers 200 with
//     {workflow_run_id, run_url, html_url}. That conditional IS correlation
//     tier 1. A fake that always returned a run id would make
//     the tier-1 path look like it works on a deployment where it is off.
//
//  2. The workflow_run row appears in the runs listing ASYNCHRONOUSLY, after
//     RunAppearDelay. GitHub creates it after accepting the dispatch, and a
//     pre-retry check arriving seconds after the original timeout can search a
//     window in which the run is real but not yet listable. A fake that
//     listed the run instantly makes ResolveOne's most likely blind spot
//     untestable.
//
//  3. Failures come back in the shapes the error mapping claims to
//     distinguish — 404, 422, a secondary rate limit that is a 403 and not a
//     429, a 5xx, and a hang that produces a client-side timeout — because
//     internal/forge/errors.go maps each to a different caller answer and a
//     single generic failure would prove none of them.
//
//  4. A declared `run-name:` is published in BOTH `name` and `display_title`
//     as the same string, and — on request — in NEITHER. Both halves were
//     measured on 2026-08-21 (RunNameFields, Run.RunNameNotApplied). The
//     second half is the only shape in which tier 2 fails while every other
//     signal says the dispatch succeeded, so a fake that cannot produce it
//     leaves the detector built for it untested.
//
// It imports nothing of ours. internal/store's tests are in-package, so a
// testutil that imported store (or forge, whose tests may also be in-package)
// would be an import cycle.
// ---------------------------------------------------------------------------

// RunNameFields are the workflow-run JSON fields this fake publishes an
// APPLIED run-name into — BOTH of them, carrying the SAME string.
//
// MEASURED, NOT ASSUMED. Against the live GitHub REST API on 2026-08-21
// (read-only):
//
//  1. `run-name:` DOES reach the REST workflow-run object.
//  2. When it applies, `name` AND `display_title` hold the same value — the
//     run-name, NOT the workflow name.
//
// Until that measurement this fake wrote the run-name into ONE named field and
// the WORKFLOW name into the other, so that a wrong forge.RunNameSource was a
// visible tier-2 failure. That shape has now been measured not to occur, and a
// fake that keeps it is a fake of a GitHub that does not exist: it fails a
// gateway that correctly reads `name` and it teaches nothing about the gateway
// that reads neither. Mimicking real behaviour outranks keeping a convenient
// tripwire — the tripwire moved into the test (forge.TestRunNameSeam...).
//
// The third measurement is the one this file still refuses to be convenient
// about: see Run.RunNameNotApplied, where a DECLARED run-name reaches NEITHER
// field.
func RunNameFields() []string { return []string{"name", "display_title"} }

// defaultHeadCommitSubject is what display_title falls back to on a run whose
// run-name was not applied. GitHub puts the head commit's subject there.
const defaultHeadCommitSubject = "Guard use of wordexp macros."

// FailMode is one injected upstream failure shape.
type FailMode string

const (
	// FailNotFound is an upstream 404 -> forge.KindWorkflowNotFound, cacheable,
	// caller sees 502 workflow_not_found.
	FailNotFound FailMode = "not_found"
	// FailUnprocessable is an upstream 422 -> forge.KindUpstreamRejected,
	// cacheable, caller sees 422 with GitHub's message.
	FailUnprocessable FailMode = "unprocessable"
	// FailSecondaryRateLimit is GitHub's secondary limit. IT IS A 403, NOT A
	// 429, and it is recognised by the documentation_url suffix plus a
	// Retry-After header — which is exactly why a fake that sent 429 would
	// silently exercise a different branch.
	FailSecondaryRateLimit FailMode = "secondary_rate_limit"
	// FailPrimaryRateLimit is the hourly quota exhausted: 403 with
	// X-RateLimit-Remaining: 0.
	FailPrimaryRateLimit FailMode = "primary_rate_limit"
	// FailServerError is an upstream 5xx: the dispatch is left `unknown`.
	FailServerError FailMode = "server_error"
	// FailUnauthorized is a rejected installation token -> credential_expired.
	FailUnauthorized FailMode = "unauthorized"
	// FailHang holds the request open until the client's own deadline fires,
	// producing a genuine client timeout rather than a synthesised one.
	FailHang FailMode = "hang"
)

// Operation names the endpoint a failure is injected on.
type Operation string

const (
	OpInstallation Operation = "installation"
	OpAccessToken  Operation = "access_token"
	OpGetRepo      Operation = "get_repo"
	OpListWorkflow Operation = "list_workflows"
	OpGetContents  Operation = "get_contents"
	OpDispatch     Operation = "dispatch"
	OpListRuns     Operation = "list_runs"
)

// DispatchRecord is one dispatch the fake accepted, recorded verbatim so a
// test can assert what actually went on the wire.
type DispatchRecord struct {
	Repo             string
	WorkflowID       int64
	Ref              string
	Inputs           map[string]any
	ReturnRunDetails bool
	// StatusReturned is the wire status this dispatch was answered with: 200
	// when run details were requested, 204 when they were not. It is
	// evidence, never a success predicate.
	StatusReturned int
	// RunID is the run this dispatch created, whether or not it was returned.
	RunID int64
	At    time.Time
}

// Run is one workflow run the fake knows about.
type Run struct {
	ID         int64
	Repo       string
	WorkflowID int64
	Path       string
	Event      string
	HeadBranch string
	// RunName is what the operator's `run-name:` template produced. When it is
	// applied it is published verbatim in EVERY field of RunNameFields().
	RunName string
	// RunNameNotApplied reproduces the third 2026-08-21 measurement: GitHub
	// SOMETIMES DOES NOT APPLY a `run-name:` that the workflow file declares
	// at the run's own head_sha. The run then carries the WORKFLOW name in
	// `name` and the head commit subject in `display_title`, so no field
	// carries the injected standby_trigger_id and tier 2 cannot match however
	// many fields it reads. The trigger falls to tier 3, is recorded
	// `inferred`, and nothing errors — a silent degradation.
	//
	// OPT-IN PER RUN. The zero value is GitHub behaving; this is the abnormal
	// case and a test asks for it explicitly. RunName is still recorded when
	// it is set, because the whole point of this shape is that the template
	// existed and the value did not appear.
	RunNameNotApplied bool
	// HeadCommitSubject is what display_title carries when no run-name
	// applies. Empty means defaultHeadCommitSubject.
	HeadCommitSubject string
	Status            string
	Conclusion        string
	Actor             string
	CreatedAt         time.Time
	// VisibleAt is when this run starts appearing in the runs listing. GitHub
	// creates the row asynchronously; before this instant the run is real and
	// NOT listable.
	VisibleAt time.Time
}

type fakeWorkflow struct {
	id      int64
	name    string
	path    string
	content string
}

type fakeRepo struct {
	defaultBranch  string
	installationID int64
	workflows      map[string]*fakeWorkflow
}

type failure struct {
	mode FailMode
	// remaining is how many more calls fail; -1 means for ever.
	remaining int
}

// GitHub is a fake GitHub REST API server.
type GitHub struct {
	srv  *httptest.Server
	stop chan struct{}

	mu             sync.Mutex
	now            func() time.Time
	runAppearDelay time.Duration
	nextRunID      int64
	nextWorkflowID int64
	repos          map[string]*fakeRepo
	runs           []*Run
	dispatches     []DispatchRecord
	fails          map[Operation]*failure
	calls          map[Operation]int
	// auth is the Authorization header of every request that reached each
	// operation, oldest first. The fake does NOT verify it — see
	// Authorizations for why the check lives in a test instead.
	auth          map[Operation][]string
	rateRemaining int
	rateLimit     int
	// runNameTemplate builds the run-name a dispatch's run carries. It mirrors
	// the setup guide's suggested `run-name: "<workflow> • ${{ inputs.standby_trigger_id }}"`.
	runNameTemplate func(workflow, triggerID string) string
	// runNameApplied is whether GitHub applies that run-name to the runs
	// subsequent dispatches create. True by default — see SetRunNameApplied.
	runNameApplied bool
}

// headBranchOf is what GitHub reports as a run's `head_branch` for a dispatch
// on ref: the SHORT name, for a tag exactly as for a branch.
//
// MEASURED 2026-08-24, read-only against api.github.com with no credentials:
// tag-triggered runs in goreleaser/goreleaser and helm/helm report
// head_branch "v2.18.0" and "v3.21.4" — the bare tag, never "refs/tags/...".
//
// This function used to strip only "refs/heads/", so a dispatch on
// refs/tags/v0.1 produced head_branch "refs/tags/v0.1", a value GitHub does
// not emit. It mattered in one direction that does not show up as a test
// failure: store.NormalizeRef and forge.BranchRef both pass a value that
// already starts with "refs/" straight through, so the fake's tag runs got a
// correctly-tagged head_branch_normalized, while a REAL tag run gets
// "refs/heads/v0.1" — branch-only by construction, as BranchRef's own comment
// says. A fake that hands the normaliser an already-correct answer can never
// exercise the case the normaliser documents as ambiguous.
func headBranchOf(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(ref, "refs/tags/"); ok {
		return rest
	}
	return ref
}

// workflowNameOf is the `name:` of the workflow a run belongs to, derived the
// same way AddWorkflow derives it.
func workflowNameOf(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, ".github/workflows/"), ".yml")
}

// NewGitHub starts a fake GitHub. Call Close when done.
//
// Defaults chosen to match production, not convenience: RunAppearDelay is
// ZERO, i.e. the run is listable immediately, because that is the common case
// and a test that wants the race sets the delay explicitly.
func NewGitHub() *GitHub {
	g := &GitHub{
		stop:           make(chan struct{}),
		now:            time.Now,
		nextRunID:      3_000_000_001,
		nextWorkflowID: 900_001,
		repos:          make(map[string]*fakeRepo),
		fails:          make(map[Operation]*failure),
		calls:          make(map[Operation]int),
		auth:           make(map[Operation][]string),
		rateRemaining:  4999,
		rateLimit:      5000,
		// GitHub applying a declared run-name is the normal case, so it is
		// the default. The measured exception is opt-in: SetRunNameApplied.
		runNameApplied: true,
		runNameTemplate: func(workflow, triggerID string) string {
			if triggerID == "" {
				return workflow
			}
			return workflow + " • " + triggerID
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", g.handleInstallation)
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", g.handleAccessToken)
	mux.HandleFunc("GET /repos/{owner}/{repo}", g.handleGetRepo)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/workflows", g.handleListWorkflows)
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", g.handleGetContents)
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/workflows/{workflow}/dispatches", g.handleDispatch)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/workflows/{workflow}/runs", g.handleListWorkflowRuns)
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs", g.handleListRepoRuns)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// An unrouted path is a fake that does not cover something the code
		// under test calls. It says so instead of answering 200 with nothing.
		g.writeError(w, http.StatusNotImplemented,
			fmt.Sprintf("testutil.GitHub does not implement %s %s", r.Method, r.URL.Path), "")
	})
	g.srv = httptest.NewServer(mux)
	return g
}

// URL is the API root to hand to forge.Config.BaseURL.
func (g *GitHub) URL() string { return g.srv.URL + "/" }

// Close releases any hanging request and shuts the server down.
func (g *GitHub) Close() {
	select {
	case <-g.stop:
	default:
		close(g.stop)
	}
	g.srv.Close()
}

// SetNow injects the fake's clock. Runs become visible relative to it, so a
// test driving a fake clock must point the fake at the same one.
func (g *GitHub) SetNow(fn func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if fn != nil {
		g.now = fn
	}
}

// SetRunAppearDelay sets how long after a dispatch the created run becomes
// listable. See the note at the top of this file: this is the window in which
// a pre-retry resolve can see nothing and be wrong about it.
func (g *GitHub) SetRunAppearDelay(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runAppearDelay = d
}

// SetRunNameTemplate overrides how a dispatched run's run-name is built.
func (g *GitHub) SetRunNameTemplate(fn func(workflow, triggerID string) string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if fn != nil {
		g.runNameTemplate = fn
	}
}

// SetRunNameApplied controls whether GitHub APPLIES the run-name to the runs
// created by SUBSEQUENT dispatches. Default true.
//
// false is the measured 2026-08-21 case in which a declared `run-name:` is not
// applied to the run at all (Run.RunNameNotApplied). The dispatched run still
// records the template's output — GitHub simply publishes neither field with
// it — so the trigger id is injected, the run exists, and tier 2 cannot see
// it. This is the shape gateway_run_name_absent_total exists to count.
func (g *GitHub) SetRunNameApplied(applied bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runNameApplied = applied
}

// SetRateRemaining sets the X-RateLimit-Remaining every response carries.
func (g *GitHub) SetRateRemaining(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rateRemaining = n
}

// AddRepo registers a repository. installationID is what
// GET /repos/{o}/{r}/installation answers.
func (g *GitHub) AddRepo(repo, defaultBranch string, installationID int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.repos[repo] = &fakeRepo{
		defaultBranch:  defaultBranch,
		installationID: installationID,
		workflows:      make(map[string]*fakeWorkflow),
	}
}

// AddWorkflow registers a workflow file and returns its numeric id. content is
// the YAML the contents endpoint serves; an empty string still yields a
// workflow whose file cannot be parsed, i.e. a COLD cache entry.
//
// KNOWN SIMPLIFICATION: the workflow's `name` — what the workflows listing
// publishes and what a run falls back to in `name` when no run-name applies —
// is derived from the FILE NAME and not from the content's own `name:` key.
// Real GitHub publishes the declared name. The consequence is that the
// 2026-08-21 probe rig, whose workflow declares `name: probe-workflow-name` in
// probe.yml precisely so the two strings are distinguishable, cannot be
// reproduced here: this fake would call it "probe". Nothing in the gateway
// compares the workflow name (tier 2 searches for the trigger id as a
// substring of the run-name fields), which is why the simplification stands;
// TestFakeKnownDivergencesFromMeasuredGitHub pins it so it stays a decision.
func (g *GitHub) AddWorkflow(repo, file, content string) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.repos[repo]
	if !ok {
		panic("testutil.GitHub: AddWorkflow before AddRepo for " + repo)
	}
	id := g.nextWorkflowID
	g.nextWorkflowID++
	r.workflows[file] = &fakeWorkflow{
		id:      id,
		name:    strings.TrimSuffix(file, ".yml"),
		path:    ".github/workflows/" + file,
		content: content,
	}
	return id
}

// AddRun injects a run that did NOT come from a dispatch through the gateway —
// the coverage `gap` population. VisibleAt is filled from CreatedAt when
// it is zero.
func (g *GitHub) AddRun(run Run) int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if run.ID == 0 {
		run.ID = g.nextRunID
		g.nextRunID++
	}
	if run.VisibleAt.IsZero() {
		run.VisibleAt = run.CreatedAt
	}
	if run.Event == "" {
		run.Event = "workflow_dispatch"
	}
	if run.Status == "" {
		run.Status = "completed"
	}
	cp := run
	g.runs = append(g.runs, &cp)
	return run.ID
}

// Fail makes every subsequent call to op fail with mode, until FailClear.
func (g *GitHub) Fail(op Operation, mode FailMode) { g.failN(op, mode, -1) }

// FailNext makes the next n calls to op fail with mode, then behave normally.
// This is what a transient upstream looks like.
func (g *GitHub) FailNext(op Operation, mode FailMode, n int) { g.failN(op, mode, n) }

func (g *GitHub) failN(op Operation, mode FailMode, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fails[op] = &failure{mode: mode, remaining: n}
}

// FailClear removes an injected failure.
func (g *GitHub) FailClear(op Operation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, op)
}

// Calls is how many times op was reached.
func (g *GitHub) Calls(op Operation) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[op]
}

// Authorizations returns the Authorization header of every request that
// reached op, oldest first. An entry is "" when the request carried none.
//
// THE FAKE DOES NOT VERIFY THE HEADER, AND THAT IS ON PURPOSE. Real GitHub
// distinguishes two credential CLASSES on these endpoints — the App JWT for
// `GET /repos/{o}/{r}/installation` and `POST /app/installations/{id}/
// access_tokens`, the installation token for everything else — and answers 401
// when they are swapped. A fake that enforced it would turn a credential-class
// mistake into a generic upstream failure in every test that touches GitHub,
// which is the loud-in-the-wrong-place failure this file's rule 3 is about.
// Instead the header is RECORDED and the check lives where it can name the
// endpoint and the class: internal/forge/credentials_test.go.
//
// Until this existed nothing in the suite read the header at all, so every
// test passed with a completely broken JWT minter.
func (g *GitHub) Authorizations(op Operation) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.auth[op]))
	copy(out, g.auth[op])
	return out
}

// Dispatches returns every dispatch the fake accepted, oldest first.
func (g *GitHub) Dispatches() []DispatchRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]DispatchRecord, len(g.dispatches))
	copy(out, g.dispatches)
	return out
}

// Runs returns every run the fake holds, including ones not yet visible.
func (g *GitHub) Runs() []Run {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Run, 0, len(g.runs))
	for _, r := range g.runs {
		out = append(out, *r)
	}
	return out
}

// SetConclusion records a run's conclusion, for the run_observed path.
func (g *GitHub) SetConclusion(runID int64, conclusion string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.runs {
		if r.ID == runID {
			r.Conclusion = conclusion
			r.Status = "completed"
		}
	}
}

// --- request handling -------------------------------------------------------

// intercept applies an injected failure. It returns true when it has already
// written the response.
func (g *GitHub) intercept(w http.ResponseWriter, r *http.Request, op Operation) bool {
	g.mu.Lock()
	g.calls[op]++
	// Recorded, never enforced. See Authorizations.
	g.auth[op] = append(g.auth[op], r.Header.Get("Authorization"))
	f := g.fails[op]
	var mode FailMode
	if f != nil {
		mode = f.mode
		if f.remaining > 0 {
			f.remaining--
			if f.remaining == 0 {
				delete(g.fails, op)
			}
		}
	}
	g.mu.Unlock()

	if mode == "" {
		return false
	}
	switch mode {
	case FailNotFound:
		g.writeError(w, http.StatusNotFound, "Not Found", "https://docs.github.com/rest")
	case FailUnprocessable:
		g.writeError(w, http.StatusUnprocessableEntity,
			"Unexpected inputs provided: [\"nope\"]", "https://docs.github.com/rest")
	case FailSecondaryRateLimit:
		// 403, not 429, recognised by the documentation_url suffix.
		w.Header().Set("Retry-After", "42")
		g.writeError(w, http.StatusForbidden,
			"You have exceeded a secondary rate limit.",
			"https://docs.github.com/rest/overview/resources-in-the-rest-api#secondary-rate-limits")
	case FailPrimaryRateLimit:
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(g.rateLimit))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(g.clock().Add(time.Hour).Unix(), 10))
		g.writeError(w, http.StatusForbidden, "API rate limit exceeded", "https://docs.github.com/rest")
	case FailServerError:
		g.writeError(w, http.StatusInternalServerError, "Server Error", "")
	case FailUnauthorized:
		g.writeError(w, http.StatusUnauthorized, "Bad credentials", "")
	case FailHang:
		// Hold until the CLIENT gives up. The point is a real
		// context.DeadlineExceeded travelling up through net/http, which is
		// what forge.isTimeout keys on; a synthesised 504 would take a
		// different branch entirely.
		//
		// The body is drained first, deliberately. net/http only starts the
		// background read that detects a closed connection once the handler
		// has consumed the request body, so a handler that blocks without
		// reading never sees its own context cancelled and wedges
		// httptest.Server.Close. The hangMax and stop cases are belt and
		// braces so a fake can never outlive the test that built it.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-g.stop:
		case <-time.After(hangMax):
		}
		return true
	default:
		g.writeError(w, http.StatusInternalServerError, "unknown injected failure "+string(mode), "")
	}
	return true
}

func (g *GitHub) clock() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now()
}

func (g *GitHub) rateHeaders(w http.ResponseWriter) {
	g.mu.Lock()
	remaining, limit := g.rateRemaining, g.rateLimit
	reset := g.now().Add(time.Hour).Unix()
	g.mu.Unlock()
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(limit))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
	w.Header().Set("X-RateLimit-Resource", "core")
}

func (g *GitHub) writeError(w http.ResponseWriter, status int, message, docURL string) {
	body := map[string]any{"message": message}
	if docURL != "" {
		body["documentation_url"] = docURL
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (g *GitHub) writeJSON(w http.ResponseWriter, status int, v any) {
	g.rateHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func repoKey(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo")
}

func (g *GitHub) repo(key string) *fakeRepo {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.repos[key]
}

func (g *GitHub) handleInstallation(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpInstallation) {
		return
	}
	repo := g.repo(repoKey(r))
	if repo == nil {
		// An unknown repo is a 404 here, which forge turns into
		// installation_not_found. Answering anything else would hide that.
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{
		"id":       repo.installationID,
		"app_id":   12345,
		"app_slug": "standpipe",
	})
}

func (g *GitHub) handleAccessToken(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpAccessToken) {
		return
	}
	g.writeJSON(w, http.StatusCreated, map[string]any{
		"token":      "ghs_fake_installation_token",
		"expires_at": g.clock().Add(time.Hour).UTC().Format(time.RFC3339),
	})
}

func (g *GitHub) handleGetRepo(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpGetRepo) {
		return
	}
	key := repoKey(r)
	repo := g.repo(key)
	if repo == nil {
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{
		"id":             1,
		"name":           r.PathValue("repo"),
		"full_name":      key,
		"default_branch": repo.defaultBranch,
	})
}

func (g *GitHub) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpListWorkflow) {
		return
	}
	repo := g.repo(repoKey(r))
	if repo == nil {
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	g.mu.Lock()
	items := make([]map[string]any, 0, len(repo.workflows))
	for _, wf := range repo.workflows {
		items = append(items, map[string]any{
			"id":    wf.id,
			"name":  wf.name,
			"path":  wf.path,
			"state": "active",
		})
	}
	g.mu.Unlock()
	// One page. Deliberately no Link header: the paging loops in
	// internal/forge stop on NextPage == 0, and a fake that paged would be
	// testing net/http's link parser rather than the gateway.
	g.writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(items),
		"workflows":   items,
	})
}

func (g *GitHub) handleGetContents(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpGetContents) {
		return
	}
	repo := g.repo(repoKey(r))
	if repo == nil {
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	path := r.PathValue("path")
	g.mu.Lock()
	var found *fakeWorkflow
	for _, wf := range repo.workflows {
		if wf.path == path {
			found = wf
			break
		}
	}
	g.mu.Unlock()
	if found == nil || found.content == "" {
		// A workflow whose file cannot be read is the COLD case: the
		// cache row is written with inputs_def NULL and validation is skipped.
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{
		"type":     "file",
		"name":     path[strings.LastIndex(path, "/")+1:],
		"path":     path,
		"sha":      "0000000000000000000000000000000000000000",
		"size":     len(found.content),
		"encoding": "base64",
		"content":  base64.StdEncoding.EncodeToString([]byte(found.content)),
	})
}

// dispatchBody is the dispatch request body.
type dispatchBody struct {
	Ref              string         `json:"ref"`
	Inputs           map[string]any `json:"inputs"`
	ReturnRunDetails *bool          `json:"return_run_details"`
}

func (g *GitHub) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpDispatch) {
		return
	}
	key := repoKey(r)
	repo := g.repo(key)
	if repo == nil {
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	var body dispatchBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		g.writeError(w, http.StatusBadRequest, "Problems parsing JSON", "")
		return
	}

	wanted := r.PathValue("workflow")
	g.mu.Lock()
	var wf *fakeWorkflow
	for file, candidate := range repo.workflows {
		if file == wanted || strconv.FormatInt(candidate.id, 10) == wanted {
			wf = candidate
			break
		}
	}
	g.mu.Unlock()
	if wf == nil {
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}

	triggerID, _ := body.Inputs["standby_trigger_id"].(string)

	g.mu.Lock()
	runID := g.nextRunID
	g.nextRunID++
	now := g.now()
	run := &Run{
		ID:         runID,
		Repo:       key,
		WorkflowID: wf.id,
		Path:       wf.path,
		Event:      "workflow_dispatch",
		HeadBranch: headBranchOf(body.Ref),
		RunName:    g.runNameTemplate(wf.name, triggerID),
		// SetRunNameApplied(false): the template ran and GitHub published it
		// in neither field.
		RunNameNotApplied: !g.runNameApplied,
		Status:            "in_progress",
		Actor:             "standpipe[bot]",
		CreatedAt:         now,
		// THE ASYNCHRONOUS ROW. GitHub accepts the dispatch and creates the
		// workflow_run afterwards; until then the run is real and unlistable.
		VisibleAt: now.Add(g.runAppearDelay),
	}
	g.runs = append(g.runs, run)

	returnDetails := body.ReturnRunDetails != nil && *body.ReturnRunDetails
	status := http.StatusNoContent
	if returnDetails {
		status = http.StatusOK
	}
	g.dispatches = append(g.dispatches, DispatchRecord{
		Repo:             key,
		WorkflowID:       wf.id,
		Ref:              body.Ref,
		Inputs:           body.Inputs,
		ReturnRunDetails: returnDetails,
		StatusReturned:   status,
		RunID:            runID,
		At:               now,
	})
	g.mu.Unlock()

	if !returnDetails {
		// 204 AND NO BODY. This is the whole of "tier 1 is off": the caller
		// gets no run id and correlation falls to tiers 2 and 3.
		g.rateHeaders(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	g.writeJSON(w, http.StatusOK, map[string]any{
		"workflow_run_id": runID,
		"run_url":         fmt.Sprintf("%sapi/v3/repos/%s/actions/runs/%d", g.srv.URL+"/", key, runID),
		"html_url":        fmt.Sprintf("https://github.com/%s/actions/runs/%d", key, runID),
	})
}

func (g *GitHub) handleListWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpListRuns) {
		return
	}
	key := repoKey(r)
	repo := g.repo(key)
	if repo == nil {
		g.writeError(w, http.StatusNotFound, "Not Found", "")
		return
	}
	wanted := r.PathValue("workflow")
	g.mu.Lock()
	var workflowID int64
	for file, candidate := range repo.workflows {
		if file == wanted || strconv.FormatInt(candidate.id, 10) == wanted {
			workflowID = candidate.id
			break
		}
	}
	g.mu.Unlock()
	g.writeRuns(w, r, key, workflowID)
}

func (g *GitHub) handleListRepoRuns(w http.ResponseWriter, r *http.Request) {
	if g.intercept(w, r, OpListRuns) {
		return
	}
	g.writeRuns(w, r, repoKey(r), 0)
}

// writeRuns applies the filters internal/forge actually sends: `event` and
// `created=>=<RFC3339>`, plus VISIBILITY.
func (g *GitHub) writeRuns(w http.ResponseWriter, r *http.Request, repo string, workflowID int64) {
	q := r.URL.Query()
	event := q.Get("event")
	// Both `created` forms internal/forge actually sends: the resolver's
	// ">=<RFC3339>" open-ended window and history discovery's "YYYY-MM-DD..YYYY-MM-DD" day
	// slice. A fake that understood only one would silently return the whole
	// history for the other, and the truncation signal is computed from the
	// size of what comes back.
	var since, until time.Time
	switch created := q.Get("created"); {
	case strings.HasPrefix(created, ">="):
		if t, err := time.Parse(time.RFC3339, strings.TrimPrefix(created, ">=")); err == nil {
			since = t
		}
	case strings.Contains(created, ".."):
		lo, hi, _ := strings.Cut(created, "..")
		if t, err := time.Parse("2006-01-02", lo); err == nil {
			since = t
		}
		if t, err := time.Parse("2006-01-02", hi); err == nil {
			until = t.Add(24*time.Hour - time.Nanosecond)
		}
	}

	g.mu.Lock()
	now := g.now()
	items := make([]map[string]any, 0, len(g.runs))
	for _, run := range g.runs {
		if run.Repo != repo {
			continue
		}
		if workflowID != 0 && run.WorkflowID != workflowID {
			continue
		}
		if event != "" && run.Event != event {
			continue
		}
		if !since.IsZero() && run.CreatedAt.Before(since) {
			continue
		}
		if !until.IsZero() && run.CreatedAt.After(until) {
			continue
		}
		if now.Before(run.VisibleAt) {
			// NOT YET LISTABLE. The run exists; GitHub has not published the
			// row. This is the branch a convenient fake deletes.
			continue
		}
		item := map[string]any{
			"id":               run.ID,
			"workflow_id":      run.WorkflowID,
			"path":             run.Path,
			"event":            run.Event,
			"head_branch":      run.HeadBranch,
			"status":           run.Status,
			"created_at":       run.CreatedAt.UTC().Format(time.RFC3339),
			"updated_at":       run.CreatedAt.UTC().Format(time.RFC3339),
			"run_started_at":   run.CreatedAt.UTC().Format(time.RFC3339),
			"actor":            map[string]any{"login": run.Actor},
			"triggering_actor": map[string]any{"login": run.Actor},
		}
		// THE RUN-NAME GOES IN BOTH FIELDS, AS THE SAME STRING. Measured
		// 2026-08-21 — see RunNameFields.
		name, title := run.RunName, run.RunName
		if run.RunNameNotApplied || run.RunName == "" {
			// No run-name on this run, either because none was declared or
			// because GitHub did not apply the one that was: `name` falls back
			// to the workflow's own name and `display_title` to the head
			// commit subject. NEITHER carries a trigger id.
			name = workflowNameOf(run.Path)
			title = run.HeadCommitSubject
			if title == "" {
				title = defaultHeadCommitSubject
			}
		}
		for _, field := range RunNameFields() {
			switch field {
			case "name":
				item[field] = name
			case "display_title":
				item[field] = title
			default:
				// RunNameFields grew a field this loop does not publish. Fail
				// loudly rather than serve a run that silently lacks it.
				panic("testutil.GitHub: RunNameFields() names a field writeRuns does not publish: " + field)
			}
		}
		if run.Conclusion != "" {
			item["conclusion"] = run.Conclusion
		} else {
			item["conclusion"] = nil
		}
		items = append(items, item)
	}
	g.mu.Unlock()

	g.writeJSON(w, http.StatusOK, map[string]any{
		"total_count":   len(items),
		"workflow_runs": items,
	})
}
