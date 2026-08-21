// Package config parses, defaults and validates standpipe.yaml.
//
// Three rules shape it:
//
//  1. An unknown field is a startup ERROR, except for the enumerated
//     known-future blocks, which are accepted with a warning and ignored. A
//     typo must not be absorbed silently.
//  2. SIGHUP reloads `callers`, `allowed_targets` and `rate_limit`
//     only. `github` and `storage` need a restart. A reload whose validation
//     fails LEAVES THE OLD CONFIGURATION IN PLACE (see Reloader).
//  3. `standpipe config validate` runs the whole of this package and has
//     no side effects, so the same code path decides both "may this process
//     start" and "is this file good".
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults for every optional key.
const (
	DefaultListen                 = ":8443"
	DefaultAdminListen            = "127.0.0.1:9090"
	DefaultIdempotencyTTL         = 24 * time.Hour
	DefaultMaxBodyBytes           = 65536
	DefaultUpstreamTimeout        = 10 * time.Second
	DefaultShutdownGrace          = 15 * time.Second
	DefaultRetentionDays          = 90
	DefaultRejectLogWindow        = 60 * time.Second
	DefaultMinFreeBytes    uint64 = 104857600
	DefaultMaxInflight            = 4
	DefaultMaxQueueWait           = 2 * time.Second
	// DefaultGitHubBaseURL is github.com's API root. parseURL in go-github
	// appends the trailing slash, so this is byte-identical to the library's
	// own default — the key changes what an operator CAN say, not what an
	// existing standpipe.yaml means.
	DefaultGitHubBaseURL   = "https://api.github.com"
	DefaultHistoryDays     = 30
	DefaultRefreshInterval = 6 * time.Hour
	DefaultLogLevel        = "info"
	DefaultLogFormat       = "json"
	DefaultMaxDriftSeconds = 2
)

// Config is the Phase 0 subset of standpipe.yaml.
type Config struct {
	Gateway        Gateway           `yaml:"gateway"`
	Storage        Storage           `yaml:"storage"`
	GitHub         GitHub            `yaml:"github"`
	AllowedTargets []AllowedTarget   `yaml:"allowed_targets"`
	Callers        map[string]Caller `yaml:"callers"`
	Inventory      Inventory         `yaml:"inventory"`
	Observability  Observability     `yaml:"observability"`
	Clock          Clock             `yaml:"clock"`

	// Path is where this configuration was read from. Not a YAML field.
	Path string `yaml:"-"`
	// Warnings are the non-fatal findings of Load: known-future blocks that
	// were ignored, and anything else the operator should see but that does
	// not stop the gateway. Never empty-and-silent — the CLI prints them.
	Warnings []string `yaml:"-"`
}

// Gateway is the `gateway:` block.
type Gateway struct {
	Listen          string    `yaml:"listen"`
	AdminListen     string    `yaml:"admin_listen"`
	TrustedProxies  []string  `yaml:"trusted_proxies"`
	IdempotencyTTL  Duration  `yaml:"idempotency_ttl"`
	MaxBodyBytes    int64     `yaml:"max_body_bytes"`
	UpstreamTimeout Duration  `yaml:"upstream_timeout"`
	ShutdownGrace   Duration  `yaml:"shutdown_grace"`
	RetentionDays   int       `yaml:"retention_days"`
	RateLimit       RateLimit `yaml:"rate_limit"`
}

// RateLimit is the per-caller limiter configuration. Every one of these is
// INBOUND and belongs to internal/httpapi; the outbound GitHub limiter is
// internal/forge/limiter.go and shares nothing with it. Its rate-limit error
// kinds are upstream GitHub errors and must never be reused for a caller 429.
type RateLimit struct {
	PerCaller       Bucket   `yaml:"per_caller"`
	Global          Bucket   `yaml:"global"`
	PreAuthIP       Bucket   `yaml:"pre_auth_ip"`
	RejectLogWindow Duration `yaml:"reject_log_window"`
}

// Bucket is one token bucket.
type Bucket struct {
	RatePerSecond float64 `yaml:"rate_per_second"`
	Burst         int     `yaml:"burst"`
}

// Storage is the `storage:` block. Not reloadable.
type Storage struct {
	SQLitePath   string `yaml:"sqlite_path"`
	MinFreeBytes uint64 `yaml:"min_free_bytes"`
}

// GitHub is the `github:` block. Not reloadable.
type GitHub struct {
	AppID          int64    `yaml:"app_id"`
	PrivateKeyPath string   `yaml:"private_key_path"`
	MaxInflight    int      `yaml:"max_inflight"`
	MaxQueueWait   Duration `yaml:"max_queue_wait"`

	// ReturnRunDetails arms correlation tier 1 (the dispatch response carries
	// the run id). It is a *bool because the documented
	// default is TRUE: an absent key must mean "default", not "off". Read it
	// through Tier1Armed and never dereference it — a zero value here is the
	// silent hazard: a zero-valued option reads as "tier 1 off" and records
	// `window` correlation on a deployment where tier 1 is on.
	ReturnRunDetails *bool `yaml:"return_run_details"`

	// BaseURL is the GitHub API root. Defaults to DefaultGitHubBaseURL.
	//
	// This key was added after the original configuration schema.
	// forge.Config has carried a BaseURL field from the start, but nothing
	// filled it, so the shipped daemon could only ever reach api.github.com.
	// That is why GitHub Enterprise Server was unsupported and why the
	// acceptance test had to rebuild the composition root instead of running
	// the binary.
	//
	// It is the API root, NOT the browser root. On GHES the two differ
	// (https://ghes.example/api/v3 against https://ghes.example) and the residual
	// GHES limit — that a run's html_url cannot be derived — is unchanged by
	// this key.
	BaseURL string `yaml:"base_url"`
}

// Tier1Armed resolves github.return_run_details (default true).
//
// This is the ONLY place the default is applied on the config side. Anything
// that needs to know whether tier 1 is armed asks here, so a deployment cannot
// dispatch with one answer and record the correlation with the other.
func (g GitHub) Tier1Armed() bool { return g.ReturnRunDetails == nil || *g.ReturnRunDetails }

// AllowedTarget is one entry of the server-side allowlist.
type AllowedTarget struct {
	Repo      string   `yaml:"repo"`
	Workflows []string `yaml:"workflows"`
	Refs      []string `yaml:"refs"`
}

// Caller is one entry of the `callers:` map.
type Caller struct {
	TokenHash string  `yaml:"token_hash"`
	TokenID   string  `yaml:"token_id"`
	Role      string  `yaml:"role"`
	Allow     []Allow `yaml:"allow"`
}

// Allow is one {repo, workflow, ref} triple of a caller's allow list.
type Allow struct {
	Repo     string `yaml:"repo"`
	Workflow string `yaml:"workflow"`
	Ref      string `yaml:"ref"`
}

// Roles.
const (
	RoleCaller = "caller"
	RoleAdmin  = "admin"
)

// Inventory is the `inventory:` block.
type Inventory struct {
	BootstrapOnStart *bool    `yaml:"bootstrap_on_start"`
	HistoryDays      int      `yaml:"history_days"`
	RefreshInterval  Duration `yaml:"refresh_interval"`
}

// Bootstrap resolves inventory.bootstrap_on_start (default true).
func (i Inventory) Bootstrap() bool { return i.BootstrapOnStart == nil || *i.BootstrapOnStart }

// Observability is the `observability:` block.
type Observability struct {
	LogLevel            string   `yaml:"log_level"`
	LogFormat           string   `yaml:"log_format"`
	MetricsEnabled      *bool    `yaml:"metrics_enabled"`
	WatchIPs            []string `yaml:"watch_ips"`
	StructuredLogFields []string `yaml:"structured_log_fields"`
}

// Metrics resolves observability.metrics_enabled (default true).
func (o Observability) Metrics() bool { return o.MetricsEnabled == nil || *o.MetricsEnabled }

// Clock is the `clock:` block. It is an operator EXPECTATION, not a
// mechanism — the gateway's own forward-jump defence is the idempotency purge guard.
type Clock struct {
	NTPRequired     *bool `yaml:"ntp_required"`
	MaxDriftSeconds int   `yaml:"max_drift_seconds"`
}

// Duration is a YAML duration string ("15s", "24h").
type Duration time.Duration

// UnmarshalYAML accepts a Go duration string. A bare number is refused: "60"
// could mean seconds or milliseconds and guessing produces a working gateway
// with the wrong window.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string like \"15s\", got %q", n.Value)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// D returns the duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Or returns d, or fallback when d is zero.
func (d Duration) Or(fallback time.Duration) time.Duration {
	if d == 0 {
		return fallback
	}
	return time.Duration(d)
}
