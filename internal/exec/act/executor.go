package act

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Executor is exec.Executor over act.
type Executor struct {
	cfg Config

	home         string
	dockerBinary string
	dockerEnv    []string
	gitBinary    string

	mu       sync.Mutex
	identity exec.Identity
	haveID   bool
}

var _ exec.Executor = (*Executor)(nil)

// New builds an Executor from one validated `backends[]` entry.
//
// It resolves the token here, once, and refuses to build without one. That is
// one half of the token rule: the token comes from CONFIGURATION and not from a
// Request, so no caller can pass a Request that omits it. The other half is
// that the process environment is not inherited at all — see
// buildInvocation.
func New(cfg Config) (*Executor, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	home := cfg.Act.HomeDir
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("act: no act.home_dir configured and the process has no home directory: %w", err)
		}
		home = h
	}
	dockerBinary, err := osexec.LookPath("docker")
	if err != nil {
		return nil, fmt.Errorf("act: docker is required for the image assertions: %w", err)
	}
	gitBinary, err := osexec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("act: git is required for U-3 and U-8: %w", err)
	}
	return &Executor{
		cfg:          cfg,
		home:         home,
		dockerBinary: dockerBinary,
		dockerEnv:    dockerEnvironment(home, cfg.Act.DockerHost),
		gitBinary:    gitBinary,
	}, nil
}

// Identity reports the engine kind, the OBSERVED version and whether it matches
// the pin. The observed version is cached: a version that changed under a
// running daemon is not something to discover halfway through a drill.
func (e *Executor) Identity() exec.Identity {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.haveID {
		return e.identity
	}
	id := exec.Identity{Kind: "act"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if v, err := e.observedVersion(ctx); err == nil {
		id.Version = v
		id.VersionPinOK = sameVersion(v, e.cfg.Act.VersionPin)
	}
	e.identity, e.haveID = id, true
	return id
}

var versionRe = regexp.MustCompile(`([0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.\-+]*)`)

func (e *Executor) observedVersion(ctx context.Context) (string, error) {
	cmd := osexec.CommandContext(ctx, e.cfg.Act.BinaryPath, "--version")
	cmd.Env = []string{
		"PATH=" + filepath.Dir(e.cfg.Act.BinaryPath) + ":" + NarrowPath,
		"HOME=" + e.home,
		"ACT_DISABLE_VERSION_CHECK=1",
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("act --version printed %q, which carries no version", strings.TrimSpace(string(out)))
	}
	return m[1], nil
}

func sameVersion(observed, pin string) bool {
	return strings.TrimPrefix(observed, "v") == strings.TrimPrefix(pin, "v")
}

// paths are the per-invocation locations.
type paths struct {
	invocationID       string
	home               string
	scratch            string // normalised workflow + event payload
	normalisedWorkflow string
	eventPayload       string
	artifactDir        string // run-specific: act's run_id is a fixed 1
}

// preparation is everything Preflight established and Run needs.
type preparation struct {
	paths paths
	inv   *invocation
	prov  exec.Provenance
}

func (p *preparation) cleanup() {
	if p == nil {
		return
	}
	if p.paths.scratch != "" {
		_ = os.RemoveAll(p.paths.scratch)
	}
}

// Preflight runs no work; it asserts that it could.
//
// Every error it returns is an *exec.RefusalError naming one of the
// fourteen refusal reasons, so the harness never has to guess which counter to move.
func (e *Executor) Preflight(ctx context.Context, req exec.Request) error {
	prep, err := e.prepare(ctx, req)
	prep.cleanup()
	return err
}

// prepare is verdict step 1 (preflight), in order.
//
// ORDER, AND THE ONE PLACE IT DEPARTS FROM THE DESIGN. The design lists
// "pin, image, platform, token, WorkDir, normalisation, local source
// mappings". Here the PLATFORM check runs before the IMAGE check, because the
// image assertion is defined over the local tags the platform mapping names —
// with an unmapped label there is no tag to assert anything about, and the
// image step would have to invent the refusal that platform_unmapped already
// spells. Everything after that is in the design's order. The swap changes
// which of two REFUSALS a broken configuration reports, never whether it is
// refused.
//
// Order buys a saved RUN, not a saved BUDGET: an early
// refusal costs the gate's 5% exactly as much as a late one.
func (e *Executor) prepare(ctx context.Context, req exec.Request) (*preparation, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}

	// 1 — version pin. The stability of act's version surface is UNMEASURED:
	// exactly one version was ever run.
	id := e.Identity()
	if id.Version == "" {
		return nil, exec.Refuse(exec.RefusalExecutorVersionMismatch,
			fmt.Errorf("act: %s did not report a version", e.cfg.Act.BinaryPath))
	}
	if !id.VersionPinOK {
		return nil, exec.Refuse(exec.RefusalExecutorVersionMismatch,
			fmt.Errorf("act: observed %s, configured pin is %s", id.Version, e.cfg.Act.VersionPin))
	}

	// 2 — platform mapping, for EVERY label the plan uses. A missing key is a
	// silent skip and a zero exit in act, which is why this is a refusal
	// and not a warning.
	for _, label := range req.RunsOn {
		if _, ok := e.cfg.Act.Platforms[label]; !ok {
			return nil, exec.Refuse(exec.RefusalPlatformUnmapped,
				fmt.Errorf("act: runs-on %q has no platforms[] entry; a missing key makes act skip the job silently and exit 0", label))
		}
	}

	// 3 — the ground (the four image assertions). Every run, never repaired here.
	images, err := e.verifyImages(ctx, req.RunsOn)
	if err != nil {
		return nil, err
	}

	// 4 — token: every call passes one explicitly.
	token, err := resolveToken(e.cfg.Act.Token.SecretRef)
	if err != nil {
		return nil, exec.Refuse(exec.RefusalTokenAbsent,
			fmt.Errorf("act: reading token.secret_ref: %w", err))
	}
	if token == "" {
		return nil, exec.Refuse(exec.RefusalTokenAbsent,
			fmt.Errorf("act: token.secret_ref %q resolved to an empty value", e.cfg.Act.Token.SecretRef))
	}

	// 5 — WorkDir at CommitSHA and clean (U-3).
	workdir, err := e.inspectWorkdir(ctx, req.WorkDir, req.CommitSHA)
	if err != nil {
		return nil, err
	}

	// 6 — normalisation (see normalise.go). Written to the executor's own scratch, never to
	// the operator's file.
	prep := &preparation{}
	src, err := os.ReadFile(filepath.Join(req.WorkDir, req.WorkflowPath))
	if err != nil {
		return nil, exec.Refuse(exec.RefusalNormalisationFailed,
			fmt.Errorf("act: reading %s: %w", req.WorkflowPath, err))
	}
	norm, err := normaliseWorkflow(src)
	if err != nil {
		return nil, exec.Refuse(exec.RefusalNormalisationFailed,
			fmt.Errorf("act: normalising %s: %w", req.WorkflowPath, err))
	}

	// 7 — local source mappings (U-8). Read BEFORE the run; unreadable refuses.
	localRepos, err := e.resolveLocalRepositories(ctx)
	if err != nil {
		return nil, err
	}

	invocationID, err := newInvocationID()
	if err != nil {
		return nil, exec.Refuse(exec.RefusalNormalisationFailed,
			fmt.Errorf("act: generating an invocation id: %w", err))
	}
	p, err := e.makePaths(invocationID, req, norm)
	if err != nil {
		prep.cleanup()
		return nil, exec.Refuse(exec.RefusalNormalisationFailed, err)
	}
	prep.paths = p

	inv, err := buildInvocation(e.cfg, req, p, token)
	if err != nil {
		prep.cleanup()
		return nil, err
	}
	prep.inv = inv

	payloadSum := sha256.Sum256(req.EventPayload)
	prep.prov = exec.Provenance{
		InvocationID:        invocationID,
		BackendName:         e.cfg.Name,
		Identity:            id,
		ArgvSHA256:          inv.sha256Hex(),
		ArgvRedacted:        inv.redacted(),
		WorkflowNormSHA256:  norm.sha256,
		WorkflowNormApplied: norm.applied,
		EventPayloadSHA256:  hex.EncodeToString(payloadSum[:]),
		Images:              images,
		LocalRepositories:   localRepos,
		Workdir:             workdir,
		ExitCode:            -1,
		Notes:               e.environmentNotes(req),
	}
	return prep, nil
}

// environmentNotes records true findings that the closed refusal set has no
// member for.
//
// The .actrc case is the reason this exists. act reads ~/.actrc, ./.actrc and
// .github/actrc, and an .actrc supplies FLAGS — which defeats the constructor's
// "the caller can neither add nor remove a flag" without touching the constructor at
// all. There is no refusal reason that names it, and inventing one is exactly
// what the closed vocabulary forbids, so it is recorded loudly and reported
// rather than absorbed.
func (e *Executor) environmentNotes(req exec.Request) []string {
	var notes []string
	candidates := []string{
		filepath.Join(e.home, ".actrc"),
		filepath.Join(req.WorkDir, ".actrc"),
		filepath.Join(req.WorkDir, ".github", "actrc"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			notes = append(notes, "actrc_present:"+c)
		}
	}
	return notes
}

func (e *Executor) makePaths(invocationID string, req exec.Request, norm normalisation) (paths, error) {
	scratch, err := os.MkdirTemp("", "standpipe-exec-")
	if err != nil {
		return paths{}, err
	}
	p := paths{
		invocationID: invocationID,
		home:         e.home,
		scratch:      scratch,
		// The name is kept so act's own log lines still identify the workflow
		// the way the operator's file does.
		normalisedWorkflow: filepath.Join(scratch, filepath.Base(req.WorkflowPath)),
		eventPayload:       filepath.Join(scratch, "event.json"),
		// Run-specific. act's run_id is a fixed 1, so two runs sharing a
		// directory read each other's artifacts [S run_context.go:1058].
		artifactDir: filepath.Join(e.cfg.Act.ArtifactServer.PathRoot, invocationID),
	}
	if err := os.WriteFile(p.normalisedWorkflow, norm.text, 0o600); err != nil {
		return p, err
	}
	if err := os.WriteFile(p.eventPayload, req.EventPayload, 0o600); err != nil {
		return p, err
	}
	if err := os.MkdirAll(p.artifactDir, 0o700); err != nil {
		return p, err
	}
	return p, nil
}

func newInvocationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Run executes and returns a structured job and step set.
//
// A non-nil error means the Result is unusable. Everything else — including
// every refusal and every harness error — comes back as a Result with a nil
// error, because a refusal is a RECORD, not a failure of this function.
func (e *Executor) Run(ctx context.Context, req exec.Request) (exec.Result, error) {
	started := time.Now().UTC()

	prep, err := e.prepare(ctx, req)
	if err != nil {
		prep.cleanup()
		return e.refusedResult(req, started, err)
	}
	defer prep.cleanup()

	res := exec.Result{
		Jobs:       []exec.JobResult{},
		Provenance: prep.prov,
		StartedAt:  started,
	}

	// U-5 — the wall-clock limit is applied HERE. act parses job-level
	// timeout-minutes and carries it as far as exactly one log.Debugf
	// [S runner.go:146], so without this a run has no bound. The effective
	// limit is min(request, configured ceiling): the request may tighten it,
	// never loosen it.
	limit := e.cfg.Act.Limits.WallClock.Std()
	if req.Deadline > 0 && req.Deadline < limit {
		limit = req.Deadline
	}
	runCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	obs := newObservation()
	exitCode, timedOut, runErr := e.invoke(runCtx, prep.inv, obs)
	res.FinishedAt = time.Now().UTC()
	res.Provenance.ExitCode = exitCode

	// The deadline verdict comes first and is NEVER `failure` (U-5). The vocabulary
	// makes it its own bucket precisely so that "never finished" has one.
	if timedOut {
		res.Jobs, _ = jobsOrEmpty(obs)
		if err := res.Finalise(exec.OutcomeDeadlineExceeded, "", ""); err != nil {
			return res, err
		}
		return res, nil
	}

	// Verdict step 4 — the unchanged part of the validator's original design. No job runs, no result is written
	// and the process exits 0, which a naive comparison scores as a perfect
	// match.
	if obs.sawUnsupportedPlatform {
		res.Jobs, _ = jobsOrEmpty(obs)
		if err := res.Finalise(exec.OutcomeHarnessError, exec.RefusalUnsupportedPlatformLine, ""); err != nil {
			return res, err
		}
		return res, nil
	}

	jobs, jobsWithoutResult := obs.jobResults()
	res.Jobs = jobs

	// U-7: a job that left records but never a result is not quietly dropped.
	if len(jobsWithoutResult) > 0 {
		res.Provenance.Notes = append(res.Provenance.Notes,
			"jobs_without_result:"+strings.Join(jobsWithoutResult, ","))
		if err := res.Finalise(exec.OutcomeHarnessError, exec.RefusalJobWithoutResult, ""); err != nil {
			return res, err
		}
		return res, nil
	}

	if len(jobs) == 0 {
		// act produced no job result at all. The exit code is deliberately not
		// consulted: U-4 says an exit code is not a result, and a measured run
		// exited 0 having run nothing. If the process itself failed to start, that
		// lands here too and the reason is recorded alongside.
		if runErr != nil {
			res.Provenance.Notes = append(res.Provenance.Notes, "process_error:"+runErr.Error())
		}
		if err := res.Finalise(exec.OutcomeHarnessError, exec.RefusalJobWithoutResult, ""); err != nil {
			return res, err
		}
		return res, nil
	}

	// The engine has no objection. This is NOT the final word: verdict steps
	// 5-9 can still downgrade it, and the run-level conclusion is derived above
	// this interface. RunConclusion is left EMPTY on purpose —
	// see exec.Result.
	res.Outcome = exec.OutcomeCompleted
	return res, nil
}

func jobsOrEmpty(obs *observation) ([]exec.JobResult, []string) {
	jobs, missing := obs.jobResults()
	if jobs == nil {
		jobs = []exec.JobResult{}
	}
	return jobs, missing
}

func (e *Executor) refusedResult(req exec.Request, started time.Time, err error) (exec.Result, error) {
	reason, ok := exec.ReasonOf(err)
	if !ok {
		// A preflight failure that named no reason is a DEFECT in this
		// package, not a state to paper over: picking a reason here would write
		// a counter that is not true.
		return exec.Result{}, err
	}
	id := e.Identity()
	res := exec.Result{
		Jobs: []exec.JobResult{},
		Provenance: exec.Provenance{
			InvocationID: "refused-" + started.UTC().Format("20060102T150405.000000000Z"),
			BackendName:  e.cfg.Name,
			Identity:     id,
			ExitCode:     -1,
			Notes:        []string{"refusal:" + err.Error()},
		},
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
	}
	if ferr := res.Finalise(exec.OutcomeRefused, reason, ""); ferr != nil {
		return res, ferr
	}
	return res, nil
}

// invoke starts act and folds both of its streams through the parser.
//
// It returns (exitCode, timedOut, err). The exit code is RECORDED and is not a
// result (U-4).
func (e *Executor) invoke(ctx context.Context, inv *invocation, obs *observation) (int, bool, error) {
	cmd := osexec.CommandContext(ctx, inv.binary, inv.realArgs()...)
	cmd.Env = inv.env
	// THE WORKING DIRECTORY IS PART OF THE INVOCATION AND THE FLAG LIST DOES NOT
	// NAME IT.
	// act's --directory defaults to ".", so with an inherited cwd it reads the
	// WRONG REPOSITORY while still being handed the right workflow file via
	// --workflows. Measured on 2026-08-22: run from elsewhere, a basic workflow
	// produced checkout=success, build=failure, test=skipped — a complete,
	// well-formed, entirely fictitious result. The first measurement set it with
	// a `cd` before the call, so this reproduces the measured invocation; it is not a flag
	// being added behind the constructor's back.
	cmd.Dir = inv.workDir
	// The whole process group is signalled. act drives containers over the
	// Docker socket, and killing only the parent leaves the run half alive on
	// a deadline — which is how a bounded run quietly becomes an unbounded one.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, false, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, false, err
	}
	if err := cmd.Start(); err != nil {
		return -1, false, err
	}

	// Both streams are parsed: which one logrus writes to is not something
	// this package should depend on, and act writes plain-text lines to the
	// other one under --json.
	var wg sync.WaitGroup
	var mu sync.Mutex
	scan := func(r io.Reader) {
		defer wg.Done()
		local := newObservation()
		_ = parseStream(r, local)
		mu.Lock()
		defer mu.Unlock()
		mergeObservations(obs, local)
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	wg.Wait()

	waitErr := cmd.Wait()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	exitCode := -1
	var ee *osexec.ExitError
	switch {
	case waitErr == nil:
		exitCode = 0
	case errors.As(waitErr, &ee):
		exitCode = ee.ExitCode()
	}
	if timedOut {
		return exitCode, true, nil
	}
	return exitCode, false, waitErr
}

// mergeObservations folds one stream's observation into another.
//
// Records for a given (job, leg) arrive on ONE stream in practice, so this is a
// union rather than an interleave; it exists so the parser never has to assume
// which stream is which.
func mergeObservations(dst, src *observation) {
	dst.nonJSONLines += src.nonJSONLines
	dst.sawUnsupportedPlatform = dst.sawUnsupportedPlatform || src.sawUnsupportedPlatform
	for _, key := range src.order {
		s := src.jobs[key]
		d, ok := dst.jobs[key]
		if !ok {
			dst.jobs[key] = s
			dst.order = append(dst.order, key)
			continue
		}
		if s.hasResult {
			d.hasResult, d.result = true, s.result
		}
		for _, id := range s.stepOrder {
			if _, exists := d.steps[id]; !exists {
				d.steps[id] = s.steps[id]
				d.stepOrder = append(d.stepOrder, id)
				continue
			}
			cur, add := d.steps[id], s.steps[id]
			if add.Conclusion != "" {
				cur.Conclusion, cur.Ran = add.Conclusion, add.Ran
			}
			if cur.Name == "" {
				cur.Name = add.Name
			}
		}
	}
}
