package act

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/fatihy101/standpipe/internal/exec"
)

// The invocation contract, as ONE normative shape.
//
// The contract lists the flags; a list cannot be coded, because it gets reassembled at
// every call site and one day someone leaves a piece out. buildInvocation is
// the only place argv exists, and it takes NO argv from its caller: the
// caller supplies a Request and a Config and can neither add a flag nor remove
// one.

// NarrowPath is the contract's "fixed, narrow PATH".
//
// act itself talks to the Docker daemon over its socket rather than shelling
// out, so this is a floor and not a dependency list. The act binary's own
// directory is prepended by buildInvocation so that a relative sibling tool
// resolves the same way it did under measurement.
const NarrowPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// redactedTokenValue stands in for the secret wherever the invocation is
// hashed or rendered. See Provenance.ArgvSHA256 for why the hash is taken over
// the redacted form.
const redactedTokenValue = "<redacted>"

// invocation is the contract's whole shape: the process environment AND the argument
// vector. Both are recorded, because DOCKER_HOST changes what ran exactly as
// much as a flag does.
type invocation struct {
	binary string
	args   []string
	// workDir is the process's working directory. It is NOT a flag and it is
	// not in the flag list, but act's --directory defaults to "." — so it
	// decides which repository is read, and a wrong one yields a complete,
	// well-formed, fictitious result. See invoke() in executor.go.
	workDir string
	// env is the COMPLETE process environment. Nothing is inherited (the
	// equivalent of `env -i`): act runs `gh auth token` silently when no token is given, so
	// an operator's GH_TOKEN / GITHUB_TOKEN / GH_CONFIG_DIR must not be
	// reachable. That is half of what makes using the operator's
	// identity "structurally impossible" [M/S — cmd/root.go:441].
	env []string
	// tokenValue is held apart so that it appears in exactly one argv element
	// and never in a rendering or a hash.
	tokenValue string
	// tokenArgIndex is the index in args of the `-s GITHUB_TOKEN=...` VALUE.
	tokenArgIndex int
}

// buildInvocation is the invocation constructor.
//
// The token comes from CONFIGURATION, not from the Request, and an empty one is
// a refusal before act is started at all — whether act falls back to
// `gh auth token` when handed an empty -s value is unmeasured, and
// this is how that gap is closed rather than assumed away.
func buildInvocation(cfg Config, req exec.Request, p paths, token string) (*invocation, error) {
	if token == "" {
		return nil, exec.Refuse(exec.RefusalTokenAbsent,
			fmt.Errorf("act: token.secret_ref %q resolved to an empty value; act is not started", cfg.Act.Token.SecretRef))
	}
	a := cfg.Act

	env := []string{
		"PATH=" + filepath.Dir(a.BinaryPath) + ":" + NarrowPath,
		"HOME=" + p.home,
		// The version check reaches the network: it pollutes what an outage
		// rehearsal measures, and adds latency to a healthy run for nothing.
		"ACT_DISABLE_VERSION_CHECK=1",
	}
	if a.DockerHost != "" {
		env = append(env, "DOCKER_HOST="+a.DockerHost)
	}

	args := []string{
		// Positional and NOT mandatory: act's usage line is
		// `act [event name to run] [flags]` and it falls silently back to
		// `on: push` when omitted. Running the wrong event is not an error, so
		// the constructor writes the event name on EVERY call
		// [V — act v0.2.89 --help].
		req.EventName,
		// The NORMALISED copy (see normalise.go), never the operator's file. Omitting this
		// defaults to the ./.github/workflows/ DIRECTORY and runs everything
		// the operator has [V — act v0.2.89 --help].
		"--workflows", p.normalisedWorkflow,
		// Given explicitly. act's default payload under workflow_dispatch is
		// EMPTY and every `github.event.*` expression is then silently null
		// — guarded steps vanish and both sides report success [M].
		"--eventpath", p.eventPayload,
	}

	// -P for EVERY configured label, in a stable order. A missing key produces
	// a silent skip and a zero exit; v0.2.89's built-in map has no
	// ubuntu-24.04 at all [S cmd/platforms.go], and ubuntu-24.04 is GitHub's
	// current ubuntu-latest [V].
	for _, label := range sortedKeys(a.Platforms) {
		args = append(args, "-P", label+"="+a.Platforms[label])
	}

	// The token: every call, explicit. The value is substituted at the end so that no
	// intermediate slice ever holds it next to the rest.
	args = append(args, "-s", "GITHUB_TOKEN="+redactedTokenValue)
	tokenArgIndex := len(args) - 1

	args = append(args,
		// A LOAD-BEARING flag, not a tidiness one: -p/--pull defaults to TRUE,
		// so omitting it makes act re-pull an image that is already local and
		// the image digest assertion slides out from under the measured run
		// [M; default V — act v0.2.89 --help].
		"--pull=false",
		// The validator's step-level channel: one structured logrus record per
		// step [M].
		"--json",
		// The step comparison's REQUIREMENT, not a setting. At the default level a step that
		// did not run emits NOTHING; under --verbose there is an explicit
		// stepResult:"skipped" and the failing `if:` in msg. The cost is ~10x
		// volume and is paid deliberately [M; S step.go:130].
		// Whether --verbose perturbs EXECUTION is unmeasured.
		"--verbose",
		// Run-specific: act's run_id is a fixed 1, so without a distinct
		// directory two runs read each other's artifacts [S run_context.go:1058].
		"--artifact-server-path", p.artifactDir,
	)

	// Addresses and ports, always explicit. act's own defaults are the HOST'S
	// LAN ADDRESS on both servers, and job containers run with network="host"
	// — so a blank address publishes them to the local network, and the fixed
	// artifact port 34567 collides between two parallel runs on one host
	// [V — act v0.2.89 --help; M]. This form has never itself been run
	// under measurement.
	args = append(args,
		"--artifact-server-addr", a.ArtifactServer.Addr,
		"--artifact-server-port", strconv.Itoa(a.ArtifactServer.Port),
		"--cache-server-addr", a.CacheServer.Addr,
		"--cache-server-port", strconv.Itoa(a.CacheServer.Port),
	)

	// A mapping rather than a seeded action cache. On a URL mismatch act runs
	// os.RemoveAll over the cache directory, so a seed destroys itself at
	// exactly the moment it is needed [S common/git/git.go:232-239].
	for _, url := range sortedKeys(a.LocalRepository) {
		args = append(args, "--local-repository", url+"="+a.LocalRepository[url])
	}

	if req.SupplyMode == exec.SupplyOffline {
		args = append(args, "--action-offline-mode")
	}

	// Without it, act's own help says it "will use host default architecture"
	// — which platform ran would depend on the host and the digest assertion
	// would mean nothing [V — act v0.2.89 --help].
	args = append(args, "--container-architecture", a.ContainerArchitecture)

	return &invocation{
		binary:        a.BinaryPath,
		args:          args,
		workDir:       req.WorkDir,
		env:           env,
		tokenValue:    token,
		tokenArgIndex: tokenArgIndex,
	}, nil
}

// realArgs returns the argument vector WITH the token substituted in. It is the
// only path by which the secret reaches a process, and nothing else may call
// it.
func (in *invocation) realArgs() []string {
	out := make([]string, len(in.args))
	copy(out, in.args)
	out[in.tokenArgIndex] = "GITHUB_TOKEN=" + in.tokenValue
	return out
}

// redacted renders the contract's whole shape — `env -i K=V ... binary args` — with the
// token replaced. This is what gets hashed and recorded.
func (in *invocation) redacted() []string {
	out := make([]string, 0, len(in.env)+len(in.args)+4)
	// The working directory is recorded with the rest of the shape: it decides
	// which repository act read, so a hash that omitted it would describe two
	// different runs identically.
	out = append(out, "cd", in.workDir, "&&", "env", "-i")
	env := append([]string(nil), in.env...)
	sort.Strings(env)
	out = append(out, env...)
	out = append(out, in.binary)
	out = append(out, in.args...)
	return out
}

// sha256Hex is the invocation's fingerprint (the ledger's argv_sha256).
//
// It is taken over the REDACTED rendering. Two runs differing only in the token
// therefore hash the same, which is the intent: the field records the SHAPE of
// what ran, and must not become an offline-checkable digest of a credential.
func (in *invocation) sha256Hex() string {
	h := sha256.New()
	for _, part := range in.redacted() {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// carriesToken reports whether argv contains an explicit -s GITHUB_TOKEN pair.
//
// The design asks for a test that fails when it does not, and names the
// guarantee as held BY A TEST rather than structurally — the same standing as
// the routing.json boundary. This predicate is what such a test
// asserts on; keeping it here means the test checks the produced argv rather
// than a copy of the intent.
func (in *invocation) carriesToken() bool {
	for i := 0; i+1 < len(in.args); i++ {
		if in.args[i] == "-s" && strings.HasPrefix(in.args[i+1], "GITHUB_TOKEN=") &&
			len(in.args[i+1]) > len("GITHUB_TOKEN=") {
			return true
		}
	}
	return false
}
