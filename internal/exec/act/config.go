package act

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// This file is the act backend's configuration surface.
//
// WHERE IT LIVES, AND WHY NOT IN internal/config. `backends` is already one of
// the known-future configuration roots: internal/config/load.go accepts the whole
// block with a warning and walks none of it. So the gateway's Config struct has
// NO field for it, and adding one would open the door to Phase 2's
// routing surface — the requirement is that ONLY the `drill --exec` path reads backends[], and
// a field on the struct standpiped loads is a field standpiped can read.
// Parsing it here instead keeps that boundary in the type system rather than in
// a test, which is strictly better than what could be managed for routing.json
// (there a test, routing_boundary_test.go, is the only guard). internal/config is therefore UNCHANGED by this slice.
//
// The cost, named: standpipe.yaml is now parsed by two packages, and
// internal/config's "unknown field is a startup error" rule does not reach
// inside `backends:`. A typo under act: is caught by Validate below and by
// nothing else, which is why Validate is strict rather than forgiving.

// Config is one `backends[]` entry of kind `act`.
type Config struct {
	Name string    `yaml:"name"`
	Kind string    `yaml:"kind"`
	Act  ActConfig `yaml:"act"`
}

// ActConfig is the `act:` block.
type ActConfig struct {
	BinaryPath string `yaml:"binary_path"`
	VersionPin string `yaml:"version_pin"`

	// Platforms maps a runs-on label to a local, single-platform tag. EVERY
	// label in the inventory must be present: a missing key produces a SILENT
	// skip in act and a zero exit, so it is a refusal here.
	Platforms map[string]string `yaml:"platforms"`

	// Images is the digest behind each local tag; the image assertion is made
	// against it.
	Images []ImageConfig `yaml:"images"`

	Token TokenConfig `yaml:"token"`

	ArtifactServer ArtifactServerConfig `yaml:"artifact_server"`
	CacheServer    ServerConfig         `yaml:"cache_server"`

	// LocalRepository maps a `uses:` URL to a local mirror (used instead of a
	// seeded action cache). What FILLS the mirror is out of scope; U-8 only records what it resolved
	// to.
	LocalRepository map[string]string `yaml:"local_repository"`

	ContainerArchitecture string `yaml:"container_architecture"`

	Limits LimitsConfig `yaml:"limits"`

	// DockerHost is the invocation's DOCKER_HOST element. The invocation
	// contract requires the value "from configuration", so the key is named
	// here. Empty means "do not set it", which leaves act on
	// its own default rather than on the operator's inherited environment,
	// because the process environment is not inherited at all.
	DockerHost string `yaml:"docker_host"`

	// HomeDir is the invocation's HOME element. Same status as DockerHost: the
	// contract requires "the executor's own HOME". Empty falls back to the
	// process owner's home directory, which is literally that, and is
	// correct for a daemon running as its own user.
	//
	// It matters more than it looks. act reads ~/.actrc, and an .actrc adds
	// FLAGS — which would defeat the constructor's "the caller can neither add
	// nor remove a flag" without touching the constructor at all. Preflight records any
	// .actrc it finds (see executor.go); there is no member of the closed
	// refusal set that names the condition, so it is recorded and reported, not
	// refused.
	HomeDir string `yaml:"home_dir"`
}

// ImageConfig is one entry of `images:`.
type ImageConfig struct {
	LocalTag string `yaml:"local_tag"`
	Source   string `yaml:"source"`
	// Digest is the platform-RESOLVED manifest digest, not the index digest.
	// An index digest is exactly what the `Architecture: ""` assertion
	// exists to catch.
	Digest   string `yaml:"digest"`
	Platform string `yaml:"platform"`
	// Materialisation is how the resolved single-platform tag is produced.
	// It is the one field that may still change after measurement, and it is
	// the name of a REPAIR rather than a preference.
	Materialisation string `yaml:"materialisation"`
}

// Materialisation modes.
const (
	// MaterialisePullByDigest pulls source@digest for the platform and tags it.
	MaterialisePullByDigest = "pull_by_digest"
	// MaterialiseCreateCommit is the first measurement's repair: docker create
	// --platform followed by docker commit. It works today and can silently
	// come back the moment the Docker version, the store type or the image
	// publisher changes; the image assertions are what notice it.
	MaterialiseCreateCommit = "create_commit"
)

// TokenConfig is `token:`. Every act call passes an explicit GITHUB_TOKEN,
// because without one act silently falls back to `gh auth token` and injects
// the operator's personal identity. The token comes from CONFIGURATION and
// never from a Request, so a caller cannot forget it.
type TokenConfig struct {
	// SecretRef is `env:NAME` or `file:PATH`. The file form exists because a token on a daemon
	// host is at least as likely to arrive as a mounted file.
	SecretRef string `yaml:"secret_ref"`
}

// ArtifactServerConfig is `artifact_server:`.
type ArtifactServerConfig struct {
	Addr string `yaml:"addr"`
	Port int    `yaml:"port"`
	// PathRoot holds one RUN-SPECIFIC subdirectory per invocation. act's
	// run_id is a fixed 1, so every invocation's artifacts land in the same
	// place unless the directory itself differs [S run_context.go:1058].
	PathRoot string `yaml:"path_root"`
}

// ServerConfig is an addr/port pair.
type ServerConfig struct {
	Addr string `yaml:"addr"`
	Port int    `yaml:"port"`
}

// LimitsConfig is `limits:`.
type LimitsConfig struct {
	// WallClock is U-5's ceiling. act parses job-level timeout-minutes and
	// carries it as far as exactly one log.Debugf [S runner.go:146], so
	// without this a run has no bound at all — and the gate's
	// arithmetic depends on there being one.
	WallClock Duration `yaml:"wall_clock"`
}

// Duration is a YAML duration in Go's syntax ("45m").
type Duration time.Duration

// UnmarshalYAML parses "45m".
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// ParseBackends reads standpipe.yaml and returns every `backends[]` entry whose
// kind is `act`.
//
// It reads the SAME file internal/config reads, and deliberately does not go
// through that package: see the note at the top of this file.
func ParseBackends(raw []byte) ([]Config, error) {
	// Only the backends subtree is decoded. Decoding the whole document with
	// KnownFields would reject every Phase 0 key, and internal/config already
	// owns that check for everything outside this block.
	var doc struct {
		Backends []Config `yaml:"backends"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("act: standpipe.yaml: %w", err)
	}
	out := make([]Config, 0, len(doc.Backends))
	for _, b := range doc.Backends {
		if b.Kind == "act" {
			out = append(out, b)
		}
	}
	return out, nil
}

// LoadBackend reads path and returns the single act backend named name. An
// empty name is accepted only when exactly one act backend exists.
func LoadBackend(path, name string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("act: %w", err)
	}
	backends, err := ParseBackends(raw)
	if err != nil {
		return Config{}, err
	}
	switch {
	case len(backends) == 0:
		return Config{}, fmt.Errorf("act: %s has no backends[] entry of kind act", path)
	case name == "" && len(backends) > 1:
		return Config{}, fmt.Errorf("act: %s has %d act backends; name one", path, len(backends))
	case name == "":
		return backends[0], nil
	}
	for _, b := range backends {
		if b.Name == name {
			return b, nil
		}
	}
	return Config{}, fmt.Errorf("act: %s has no act backend named %q", path, name)
}

// Validate rejects a configuration that could not produce an honest run.
//
// Every field here is an INPUT TO AN ASSERTION: a
// wrong value refuses the run, it does not bend it. So the errors are startup
// errors, in the same spirit as the gateway's unknown-field rule.
func (c Config) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("act: backends[].name is required — the record shows this name")
	}
	if c.Kind != "act" {
		return fmt.Errorf("act: backend %q has kind %q, not act", c.Name, c.Kind)
	}
	a := c.Act
	if a.BinaryPath == "" {
		return fmt.Errorf("act: %s: act.binary_path is required", c.Name)
	}
	if a.VersionPin == "" {
		return fmt.Errorf("act: %s: act.version_pin is required — the stability of act's version surface is UNMEASURED", c.Name)
	}
	if len(a.Platforms) == 0 {
		return fmt.Errorf("act: %s: act.platforms is empty — a call with no -P falls into an interactive TTY prompt and dies on EOF", c.Name)
	}
	if a.ContainerArchitecture == "" {
		return fmt.Errorf("act: %s: act.container_architecture must not be empty — without it act uses the host default and the digest assertion means nothing", c.Name)
	}
	if !strings.Contains(a.ContainerArchitecture, "/") {
		return fmt.Errorf("act: %s: act.container_architecture %q is not os/arch", c.Name, a.ContainerArchitecture)
	}
	if a.Token.SecretRef == "" {
		return fmt.Errorf("act: %s: act.token.secret_ref is required — without it act falls back to the operator's gh auth token", c.Name)
	}
	if _, err := parseSecretRef(a.Token.SecretRef); err != nil {
		return fmt.Errorf("act: %s: %w", c.Name, err)
	}
	if a.Limits.WallClock <= 0 {
		return fmt.Errorf("act: %s: act.limits.wall_clock must be positive (U-5) — act ignores job-level timeout-minutes, so an unbounded run has no bucket at all", c.Name)
	}

	// Every mapped tag needs an images[] entry, or the image assertion has nothing to assert
	// against and the ground is unverified by construction.
	byTag := map[string]ImageConfig{}
	for _, img := range a.Images {
		if img.LocalTag == "" {
			return fmt.Errorf("act: %s: images[].local_tag is required", c.Name)
		}
		if _, dup := byTag[img.LocalTag]; dup {
			return fmt.Errorf("act: %s: images[] has two entries for local_tag %q", c.Name, img.LocalTag)
		}
		if img.Source == "" {
			return fmt.Errorf("act: %s: images[%s].source is required", c.Name, img.LocalTag)
		}
		if !strings.HasPrefix(img.Digest, "sha256:") || len(img.Digest) != len("sha256:")+64 {
			return fmt.Errorf("act: %s: images[%s].digest %q is not a sha256 digest", c.Name, img.LocalTag, img.Digest)
		}
		if !strings.Contains(img.Platform, "/") {
			return fmt.Errorf("act: %s: images[%s].platform %q is not os/arch", c.Name, img.LocalTag, img.Platform)
		}
		switch img.Materialisation {
		case MaterialisePullByDigest, MaterialiseCreateCommit:
		case "":
			return fmt.Errorf("act: %s: images[%s].materialisation is required (%s|%s)", c.Name, img.LocalTag, MaterialisePullByDigest, MaterialiseCreateCommit)
		default:
			return fmt.Errorf("act: %s: images[%s].materialisation %q is neither %s nor %s", c.Name, img.LocalTag, img.Materialisation, MaterialisePullByDigest, MaterialiseCreateCommit)
		}
		byTag[img.LocalTag] = img
	}
	for _, label := range sortedKeys(a.Platforms) {
		tag := a.Platforms[label]
		if tag == "" {
			return fmt.Errorf("act: %s: platforms[%s] is empty", c.Name, label)
		}
		if _, ok := byTag[tag]; !ok {
			return fmt.Errorf("act: %s: platforms[%s] maps to %q, which has no images[] entry — the image assertion could not check anything about it", c.Name, label, tag)
		}
	}
	for _, url := range sortedKeys(a.LocalRepository) {
		if !filepath.IsAbs(a.LocalRepository[url]) {
			return fmt.Errorf("act: %s: local_repository[%s] must be an absolute path", c.Name, url)
		}
	}
	if a.ArtifactServer.PathRoot == "" {
		return fmt.Errorf("act: %s: act.artifact_server.path_root is required — the artifact server is OFF without --artifact-server-path and upload-artifact then fails hard [S run_context.go:1058]", c.Name)
	}
	if a.ArtifactServer.Addr == "" || a.CacheServer.Addr == "" {
		return fmt.Errorf("act: %s: artifact_server.addr and cache_server.addr must both be set — act's own default is the host's LAN address, not localhost, so leaving them blank publishes the servers to the local network [V — act v0.2.89 --help]", c.Name)
	}
	if a.ArtifactServer.Port <= 0 || a.ArtifactServer.Port > 65535 {
		return fmt.Errorf("act: %s: artifact_server.port %d out of range", c.Name, a.ArtifactServer.Port)
	}
	if a.CacheServer.Port <= 0 || a.CacheServer.Port > 65535 {
		// 0 is act's own "pick a random free port" for the cache server, and
		// it is refused here on purpose: a port nobody chose is a port nobody
		// recorded, and the recorded argv hash would stop describing what ran.
		return fmt.Errorf("act: %s: cache_server.port %d out of range (0 means 'random free port' to act and is not recordable)", c.Name, a.CacheServer.Port)
	}
	return nil
}

// ImageFor returns the images[] entry behind a runs-on label.
func (a ActConfig) ImageFor(label string) (ImageConfig, bool) {
	tag, ok := a.Platforms[label]
	if !ok {
		return ImageConfig{}, false
	}
	for _, img := range a.Images {
		if img.LocalTag == tag {
			return img, true
		}
	}
	return ImageConfig{}, false
}

type secretRef struct {
	scheme string
	value  string
}

func parseSecretRef(s string) (secretRef, error) {
	scheme, value, ok := strings.Cut(s, ":")
	if !ok || value == "" {
		return secretRef{}, fmt.Errorf("token.secret_ref %q is not scheme:value", s)
	}
	switch scheme {
	case "env", "file":
		return secretRef{scheme: scheme, value: value}, nil
	default:
		return secretRef{}, fmt.Errorf("token.secret_ref scheme %q is neither env nor file", scheme)
	}
}

// resolveToken reads the token. Empty is a refusal and act is NEVER
// started.
//
// The emptiness rule is not a convenience. Whether act falls back to
// `gh auth token` when -s GITHUB_TOKEN= is passed with an EMPTY value has not
// been measured, so this is how that gap is closed.
func resolveToken(ref string) (string, error) {
	r, err := parseSecretRef(ref)
	if err != nil {
		return "", err
	}
	switch r.scheme {
	case "env":
		return strings.TrimSpace(os.Getenv(r.value)), nil
	case "file":
		b, err := os.ReadFile(r.value)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	return "", fmt.Errorf("unreachable")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
