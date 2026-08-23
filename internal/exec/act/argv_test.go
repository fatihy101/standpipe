package act

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/exec"
)

// The invocation contract. buildInvocation is the only place argv exists and
// takes NO argv from its caller, so everything below is checked on the PRODUCED
// vector rather than on a copy of the intent.

func testConfig() Config {
	return Config{
		Name: "act-local",
		Kind: "act",
		Act: ActConfig{
			BinaryPath: "/opt/act/bin/act",
			VersionPin: "0.2.89",
			Platforms: map[string]string{
				"ubuntu-24.04": "standpipe/ubuntu:24.04",
				"ubuntu-22.04": "standpipe/ubuntu:22.04",
			},
			Images: []ImageConfig{
				{LocalTag: "standpipe/ubuntu:24.04", Source: "catthehacker/ubuntu:act-24.04",
					Digest: "sha256:" + strings.Repeat("a", 64), Platform: "linux/amd64",
					Materialisation: MaterialisePullByDigest},
				{LocalTag: "standpipe/ubuntu:22.04", Source: "catthehacker/ubuntu:act-22.04",
					Digest: "sha256:" + strings.Repeat("b", 64), Platform: "linux/amd64",
					Materialisation: MaterialiseCreateCommit},
			},
			Token:                 TokenConfig{SecretRef: "env:STANDPIPE_ACT_TOKEN"},
			ArtifactServer:        ArtifactServerConfig{Addr: "127.0.0.1", Port: 34567, PathRoot: "/var/lib/standpipe/artifacts"},
			CacheServer:           ServerConfig{Addr: "127.0.0.1", Port: 34568},
			LocalRepository:       map[string]string{"https://github.com/actions/checkout": "/srv/mirrors/checkout"},
			ContainerArchitecture: "linux/amd64",
			Limits:                LimitsConfig{WallClock: Duration(45 * time.Minute)},
			DockerHost:            "unix:///var/run/docker.sock",
			HomeDir:               "/var/lib/standpipe",
		},
	}
}

func testRequest() exec.Request {
	return exec.Request{
		Repo:         "owner/name",
		CommitSHA:    strings.Repeat("c", 40),
		WorkflowPath: ".github/workflows/test.yml",
		EventName:    "push",
		EventPayload: []byte(`{"ref":"refs/heads/main"}`),
		RunsOn:       []string{"ubuntu-24.04"},
		WorkDir:      "/srv/work/run-1",
		SupplyMode:   exec.SupplyOnline,
		Purpose:      exec.PurposeDrill,
		Deadline:     time.Minute,
	}
}

func testPaths() paths {
	return paths{
		invocationID:       "inv-1",
		home:               "/var/lib/standpipe",
		scratch:            "/tmp/scratch",
		normalisedWorkflow: "/tmp/scratch/test.yml",
		eventPayload:       "/tmp/scratch/event.json",
		artifactDir:        "/var/lib/standpipe/artifacts/inv-1",
	}
}

func mustBuild(t *testing.T, cfg Config, req exec.Request, token string) *invocation {
	t.Helper()
	in, err := buildInvocation(cfg, req, testPaths(), token)
	if err != nil {
		t.Fatalf("buildInvocation: %v", err)
	}
	return in
}

// TestArgvCarriesTheTokenOnEveryCall is the token guarantee's own test.
//
// The design names this guarantee as one held BY A TEST rather than
// structurally, the same standing as the routing.json boundary — and until now
// no such test existed. The whole claim is that act cannot reach the
// operator's credential: `-s GITHUB_TOKEN=` is explicit on every call, and
// `env -i` is the other half.
func TestArgvCarriesTheTokenOnEveryCall(t *testing.T) {
	for _, mode := range []exec.SupplyMode{exec.SupplyOnline, exec.SupplyOffline} {
		req := testRequest()
		req.SupplyMode = mode
		in := mustBuild(t, testConfig(), req, "ghp_realtoken")
		if !in.carriesToken() {
			t.Fatalf("supply mode %s: argv carries no explicit -s GITHUB_TOKEN pair: %v", mode, in.args)
		}
		// And the real value reaches the process at exactly one index.
		real := in.realArgs()
		n := 0
		for _, a := range real {
			if strings.Contains(a, "ghp_realtoken") {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("the token appears in %d argv elements, want exactly 1: %v", n, real)
		}
		if real[in.tokenArgIndex] != "GITHUB_TOKEN=ghp_realtoken" {
			t.Fatalf("tokenArgIndex points at %q", real[in.tokenArgIndex])
		}
	}
}

// TestAnEmptyTokenIsARefusalBeforeActStarts — whether act falls back to
// `gh auth token` when handed an empty -s value is UNMEASURED, and this
// is how that gap is closed rather than assumed away.
func TestAnEmptyTokenIsARefusalBeforeActStarts(t *testing.T) {
	_, err := buildInvocation(testConfig(), testRequest(), testPaths(), "")
	if err == nil {
		t.Fatal("buildInvocation produced an invocation with an empty token")
	}
	reason, ok := exec.ReasonOf(err)
	if !ok || reason != exec.RefusalTokenAbsent {
		t.Fatalf("the refusal does not name token_absent: %q, %v (%v)", reason, ok, err)
	}
	var re *exec.RefusalError
	if !errors.As(err, &re) {
		t.Fatalf("want *exec.RefusalError, got %T", err)
	}
}

// TestTheTokenIsNeverInTheRecord — the argv hash is taken over the REDACTED
// rendering. Two runs differing only in the token hash identically: the field
// records the SHAPE of the invocation and must not become an offline-checkable
// digest of a credential.
func TestTheTokenIsNeverInTheRecord(t *testing.T) {
	a := mustBuild(t, testConfig(), testRequest(), "ghp_aaaaaaaaaaaa")
	b := mustBuild(t, testConfig(), testRequest(), "ghp_bbbbbbbbbbbb")

	for _, part := range a.redacted() {
		if strings.Contains(part, "ghp_") {
			t.Fatalf("the redacted rendering carries the token: %q", part)
		}
	}
	if !containsElem(a.redacted(), "GITHUB_TOKEN="+redactedTokenValue) {
		t.Fatalf("the redacted rendering lost the token ELEMENT; the shape must still show that -s was passed: %v", a.redacted())
	}
	if a.sha256Hex() != b.sha256Hex() {
		t.Fatal("two runs differing only in the token hash differently; the field has become a digest of the credential")
	}
	if len(a.sha256Hex()) != 64 {
		t.Fatalf("sha = %q", a.sha256Hex())
	}
}

// TestTheWorkingDirectoryIsInsideTheHash is the 2026-08-22 measurement.
//
// The contract's flag list does not mention the process working directory, and
// act's --directory defaults to ".". Running from the wrong directory produced a
// complete, well-formed Result describing a run of a repository that was never
// involved — three steps, a job conclusion, a derivable run conclusion. A hash
// that omitted the directory would describe two such runs identically.
func TestTheWorkingDirectoryIsInsideTheHash(t *testing.T) {
	a := testRequest()
	b := testRequest()
	b.WorkDir = "/srv/work/some-other-repo"

	inA := mustBuild(t, testConfig(), a, "tok")
	inB := mustBuild(t, testConfig(), b, "tok")

	if inA.sha256Hex() == inB.sha256Hex() {
		t.Fatal("two runs in different repositories hash identically; the working directory has fallen out of the invocation record")
	}
	if inA.workDir != a.WorkDir {
		t.Fatalf("workDir = %q, want %q", inA.workDir, a.WorkDir)
	}
	// The rendering says so out loud, because that is what an investigation
	// reads.
	r := inA.redacted()
	if len(r) < 2 || r[0] != "cd" || r[1] != a.WorkDir {
		t.Fatalf("the rendering does not open with the working directory: %v", r[:min(4, len(r))])
	}
	// The argument vector itself carries no --directory; the cwd is the input.
	if containsElem(inA.args, "--directory") || containsElem(inA.args, "-C") {
		t.Fatalf("argv gained a --directory flag behind the constructor's back: %v", inA.args)
	}
}

// TestArgvIsStableAcrossBuilds — the hash is an identity, so a map iteration
// order leaking into argv would make the same invocation hash differently from
// one process to the next.
func TestArgvIsStableAcrossBuilds(t *testing.T) {
	first := mustBuild(t, testConfig(), testRequest(), "tok").sha256Hex()
	for i := 0; i < 50; i++ {
		if got := mustBuild(t, testConfig(), testRequest(), "tok").sha256Hex(); got != first {
			t.Fatalf("build %d hashed %s, first build hashed %s — a map iteration order is reaching argv", i, got, first)
		}
	}
}

// TestArgvHoldsTheLoadBearingFlags. Each of these is a flag whose ABSENCE is
// silent: act's own default is the dangerous one and nothing errors.
func TestArgvHoldsTheLoadBearingFlags(t *testing.T) {
	cfg := testConfig()
	in := mustBuild(t, cfg, testRequest(), "tok")

	// The event name is POSITIONAL and not mandatory to act: omitted, act falls
	// silently back to `on: push`. Running the wrong event is not an error.
	if len(in.args) == 0 || in.args[0] != "push" {
		t.Fatalf("argv does not open with the event name: %v", in.args)
	}

	for _, pair := range [][2]string{
		// The NORMALISED copy, never the operator's file. Omitting --workflows
		// defaults to the whole .github/workflows/ DIRECTORY.
		{"--workflows", testPaths().normalisedWorkflow},
		// act's default payload under workflow_dispatch is EMPTY and every
		// github.event.* expression is then silently null.
		{"--eventpath", testPaths().eventPayload},
		// act's run_id is a fixed 1, so without a distinct directory two runs
		// read each other's artifacts.
		{"--artifact-server-path", testPaths().artifactDir},
		// act's own defaults here are the HOST'S LAN ADDRESS on both servers,
		// and job containers run with network="host".
		{"--artifact-server-addr", "127.0.0.1"},
		{"--artifact-server-port", "34567"},
		{"--cache-server-addr", "127.0.0.1"},
		{"--cache-server-port", "34568"},
		// Without it act "will use host default architecture" and the digest
		// assertion means nothing.
		{"--container-architecture", "linux/amd64"},
	} {
		if !hasFlagValue(in.args, pair[0], pair[1]) {
			t.Errorf("argv is missing %s %s: %v", pair[0], pair[1], in.args)
		}
	}

	// -p/--pull defaults to TRUE, so omitting --pull=false makes act re-pull an
	// image that is already local and the digest assertion slides out from
	// under the measured run.
	for _, flag := range []string{"--pull=false", "--json", "--verbose"} {
		if !containsElem(in.args, flag) {
			t.Errorf("argv is missing %s: %v", flag, in.args)
		}
	}

	// -P for EVERY configured label, in a stable order. A missing key produces
	// a silent skip and a zero exit.
	for label, tag := range cfg.Act.Platforms {
		if !hasFlagValue(in.args, "-P", label+"="+tag) {
			t.Errorf("argv is missing -P %s=%s: %v", label, tag, in.args)
		}
	}
	// A mapping, not a seeded action cache.
	if !hasFlagValue(in.args, "--local-repository", "https://github.com/actions/checkout=/srv/mirrors/checkout") {
		t.Errorf("argv is missing the --local-repository mapping: %v", in.args)
	}
}

// TestOfflineModeIsTheOnlyDifferenceTheSupplyModeMakes — SupplyMode is the
// gate's score axis, and a run recorded `offline` that did not carry
// --action-offline-mode is a rehearsal that rehearsed nothing.
func TestOfflineModeIsTheOnlyDifferenceTheSupplyModeMakes(t *testing.T) {
	online := testRequest()
	offline := testRequest()
	offline.SupplyMode = exec.SupplyOffline

	on := mustBuild(t, testConfig(), online, "tok")
	off := mustBuild(t, testConfig(), offline, "tok")

	if containsElem(on.args, "--action-offline-mode") {
		t.Error("an ONLINE run carries --action-offline-mode")
	}
	if !containsElem(off.args, "--action-offline-mode") {
		t.Fatalf("an OFFLINE run does not carry --action-offline-mode: %v", off.args)
	}
	if on.sha256Hex() == off.sha256Hex() {
		t.Fatal("the two supply modes produce the same argv hash; the record cannot tell an outage rehearsal from an ordinary run")
	}
}

// TestEnvironmentIsCompleteAndNarrow — the equivalent of `env -i`. act runs `gh auth token`
// silently when no token is given, so an operator's GH_TOKEN / GITHUB_TOKEN /
// GH_CONFIG_DIR must not be reachable; nothing is inherited.
func TestEnvironmentIsCompleteAndNarrow(t *testing.T) {
	in := mustBuild(t, testConfig(), testRequest(), "tok")

	want := map[string]string{
		"PATH":                      "/opt/act/bin:" + NarrowPath,
		"HOME":                      "/var/lib/standpipe",
		"ACT_DISABLE_VERSION_CHECK": "1",
		"DOCKER_HOST":               "unix:///var/run/docker.sock",
	}
	if len(in.env) != len(want) {
		t.Fatalf("env = %v, want exactly %d entries", in.env, len(want))
	}
	got := map[string]string{}
	for _, kv := range in.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry %q is not K=V", kv)
		}
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}

	// The act binary's own directory is PREPENDED so a relative sibling tool
	// resolves the way it did under measurement.
	if !strings.HasPrefix(got["PATH"], "/opt/act/bin:") {
		t.Errorf("PATH does not start with the act binary's directory: %q", got["PATH"])
	}

	// DOCKER_HOST changes what ran exactly as much as a flag does, so it is
	// inside the hash.
	cfg := testConfig()
	cfg.Act.DockerHost = "tcp://10.0.0.5:2375"
	if mustBuild(t, cfg, testRequest(), "tok").sha256Hex() == in.sha256Hex() {
		t.Fatal("DOCKER_HOST is outside the argv hash; two runs against different daemons record identically")
	}
	// Empty means "do not set it" — not an empty value act would read as one.
	cfg.Act.DockerHost = ""
	for _, kv := range mustBuild(t, cfg, testRequest(), "tok").env {
		if strings.HasPrefix(kv, "DOCKER_HOST=") {
			t.Errorf("an unset docker_host produced %q", kv)
		}
	}
}

// TestConfigValidateRefusesWhatWouldRunDishonestly walks the configuration
// rule: every field here is an input to an ASSERTION, so a wrong value
// refuses the run rather than bending it.
func TestConfigValidateRefusesWhatWouldRunDishonestly(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatalf("a complete configuration was rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
	}{
		{"no platforms — a call with no -P dies on an EOF prompt", func(c *Config) { c.Act.Platforms = nil }},
		{"a mapped tag with no images[] entry — the image assertion checks nothing", func(c *Config) {
			c.Act.Platforms["ubuntu-24.04"] = "some/other:tag"
		}},
		{"an index digest is not a resolved digest", func(c *Config) { c.Act.Images[0].Digest = "sha256:short" }},
		{"a materialisation nobody named", func(c *Config) { c.Act.Images[0].Materialisation = "" }},
		{"no container_architecture — the host default decides", func(c *Config) { c.Act.ContainerArchitecture = "" }},
		{"an architecture that is not os/arch", func(c *Config) { c.Act.ContainerArchitecture = "amd64" }},
		{"no wall clock — the gate's arithmetic has no bound", func(c *Config) { c.Act.Limits.WallClock = 0 }},
		{"no version pin", func(c *Config) { c.Act.VersionPin = "" }},
		{"no token secret_ref", func(c *Config) { c.Act.Token.SecretRef = "" }},
		{"a secret_ref scheme that is neither env nor file", func(c *Config) { c.Act.Token.SecretRef = "vault:x" }},
		{"no artifact path_root — the artifact server is simply OFF", func(c *Config) { c.Act.ArtifactServer.PathRoot = "" }},
		{"a blank server addr publishes to the LAN", func(c *Config) { c.Act.ArtifactServer.Addr = "" }},
		{"cache port 0 means 'random' and is not recordable", func(c *Config) { c.Act.CacheServer.Port = 0 }},
		{"a relative local_repository path", func(c *Config) {
			c.Act.LocalRepository["https://github.com/actions/checkout"] = "mirrors/checkout"
		}},
		{"two images[] entries for one tag", func(c *Config) {
			c.Act.Images = append(c.Act.Images, c.Act.Images[0])
		}},
		{"a kind that is not act", func(c *Config) { c.Kind = "docker" }},
		{"no name — the record shows this name", func(c *Config) { c.Name = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.mut(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
}

func containsElem(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

func hasFlagValue(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
