package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// schema is the set of keys the configuration defines. It exists so the
// three-way rule —
// known / known-future / unknown — can be applied per key with an exact path in
// the message, which yaml.v3's KnownFields cannot express: KnownFields is all
// or nothing and its error text is not a path.
type schema struct {
	fields map[string]*schema
	// anyKey describes a mapping whose keys the operator chooses (callers).
	anyKey *schema
	// elem describes a sequence's element.
	elem *schema
	// leaf means "do not descend": a scalar, or a free-form value.
	leaf bool
}

func leaf() *schema { return &schema{leaf: true} }

func fields(f map[string]*schema) *schema { return &schema{fields: f} }

func seq(elem *schema) *schema { return &schema{elem: elem} }

// currentSchema is the Phase 0 subset.
func currentSchema() *schema {
	bucket := fields(map[string]*schema{
		"rate_per_second": leaf(),
		"burst":           leaf(),
	})
	allow := seq(fields(map[string]*schema{
		"repo":     leaf(),
		"workflow": leaf(),
		"ref":      leaf(),
	}))
	return fields(map[string]*schema{
		"gateway": fields(map[string]*schema{
			"listen":           leaf(),
			"admin_listen":     leaf(),
			"trusted_proxies":  leaf(),
			"idempotency_ttl":  leaf(),
			"max_body_bytes":   leaf(),
			"upstream_timeout": leaf(),
			"shutdown_grace":   leaf(),
			"retention_days":   leaf(),
			"rate_limit": fields(map[string]*schema{
				"per_caller":        bucket,
				"global":            bucket,
				"pre_auth_ip":       bucket,
				"reject_log_window": leaf(),
			}),
		}),
		"storage": fields(map[string]*schema{
			"sqlite_path":    leaf(),
			"min_free_bytes": leaf(),
		}),
		"github": fields(map[string]*schema{
			"app_id":             leaf(),
			"private_key_path":   leaf(),
			"max_inflight":       leaf(),
			"max_queue_wait":     leaf(),
			"return_run_details": leaf(),
			"base_url":           leaf(),
		}),
		"allowed_targets": seq(fields(map[string]*schema{
			"repo":      leaf(),
			"workflows": leaf(),
			"refs":      leaf(),
		})),
		"callers": {anyKey: fields(map[string]*schema{
			"token_hash": leaf(),
			"token_id":   leaf(),
			"role":       leaf(),
			"allow":      allow,
		})},
		"inventory": fields(map[string]*schema{
			"bootstrap_on_start": leaf(),
			"history_days":       leaf(),
			"refresh_interval":   leaf(),
		}),
		"observability": fields(map[string]*schema{
			"log_level":             leaf(),
			"log_format":            leaf(),
			"metrics_enabled":       leaf(),
			"watch_ips":             leaf(),
			"structured_log_fields": leaf(),
		}),
		"clock": fields(map[string]*schema{
			"ntp_required":      leaf(),
			"max_drift_seconds": leaf(),
		}),
	})
}

// futureRoots is the known-future list: accepted with a warning, ignored.
//
// The whole block is skipped rather than walked key by key, and the reason is
// OWNERSHIP, not absence. It used to be written as absence — "every key under
// it belongs to a phase that has not been designed yet, so this package has
// nothing to check a sub-key against" — and that stopped being true of every
// member the moment `drill` joined: Phase 1 designs its four keys precisely,
// `drill.timezone` down to "required, no default", and internal/shadow
// already ships the drill_day_zone table. Phase 0 could check
// those sub-keys. It does not, because a schema validated here would be a
// SECOND COPY of one the drill package owns, and of two copies the one nobody
// updates is the one in the phase that does not read the values.
//
// Erroring is off the table for a separate reason that does hold for every
// member: it would make the operator delete a block the roadmap tells them to
// keep, and a Phase 0 gateway sharing a file with a Phase 1 installation
// would refuse to start.
var futureRoots = map[string]string{
	"backends": "backends[].kind: act — Phase 1",
	// One key, not the drill block's set of four. `drill.timezone` is the key with no
	// default — it decides the day boundary, and therefore which run lands in
	// which day and how the 14/21 window is counted — so it is the one worth
	// naming. Listing all four would put a hand copy of a Phase 1 key set in a
	// Phase 0 file, which is the drift this map is trying not to have.
	"drill":     "drill.timezone — Phase 1",
	"policy":    "policy.hysteresis / policy.probe / policy.on_no_eligible_backend — Phase 2",
	"workflows": "workflows[].routable — Phase 2, workflows[].shadow — Phase 1",
	"remotes":   "remotes — Phase 3",
	"lease_on":  "lease_on — Phase 4/5",
}

// Load reads, defaults and validates a configuration file.
//
// It has no side effects beyond reading files, so `standpipe config validate` and
// gateway startup run exactly the same code and cannot disagree.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg.Path = path
	return cfg, nil
}

// Parse is Load without the file read.
func Parse(raw []byte) (*Config, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var warnings []string
	var unknown []string
	if root := documentRoot(&doc); root != nil {
		if root.Kind != yaml.MappingNode {
			return nil, errors.New("config: the top level of standpipe.yaml must be a mapping")
		}
		walk(root, currentSchema(), "", &unknown, &warnings)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		// A completely unknown field is a startup error. An accidental
		// key is noticed rather than absorbed.
		return nil, fmt.Errorf("config: unknown field(s): %s", strings.Join(unknown, ", "))
	}

	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg.Warnings = warnings
	cfg.applyDefaults()
	// Canonicalisation runs BEFORE Validate so the error messages Validate
	// prints quote the same strings the gateway will actually use, and AFTER
	// applyDefaults so it is part of "what Load produces" rather than a step a
	// caller could skip.
	cfg.Warnings = append(cfg.Warnings, cfg.canonicaliseWatchIPs()...)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func documentRoot(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	if n.Kind == 0 {
		return nil
	}
	return n
}

// walk compares a YAML node against the schema, collecting unknown paths and
// known-future warnings.
func walk(n *yaml.Node, s *schema, prefix string, unknown, warnings *[]string) {
	if s == nil || s.leaf || n == nil {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			val := n.Content[i+1]
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			switch {
			case s.anyKey != nil:
				walk(val, s.anyKey, path, unknown, warnings)
			case s.fields[key] != nil:
				walk(val, s.fields[key], path, unknown, warnings)
			case prefix == "" && futureRoots[key] != "":
				*warnings = append(*warnings,
					fmt.Sprintf("%s: known-future field, accepted and ignored in Phase 0 (%s)", path, futureRoots[key]))
			default:
				*unknown = append(*unknown, path+fmt.Sprintf(" (line %d)", n.Content[i].Line))
			}
		}
	case yaml.SequenceNode:
		for i, item := range n.Content {
			walk(item, s.elem, fmt.Sprintf("%s[%d]", prefix, i), unknown, warnings)
		}
	}
}

func (c *Config) applyDefaults() {
	g := &c.Gateway
	if g.Listen == "" {
		g.Listen = DefaultListen
	}
	if g.AdminListen == "" {
		g.AdminListen = DefaultAdminListen
	}
	if g.IdempotencyTTL == 0 {
		g.IdempotencyTTL = Duration(DefaultIdempotencyTTL)
	}
	if g.MaxBodyBytes == 0 {
		g.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if g.UpstreamTimeout == 0 {
		g.UpstreamTimeout = Duration(DefaultUpstreamTimeout)
	}
	if g.ShutdownGrace == 0 {
		g.ShutdownGrace = Duration(DefaultShutdownGrace)
	}
	if g.RetentionDays == 0 {
		g.RetentionDays = DefaultRetentionDays
	}
	if g.RateLimit.RejectLogWindow == 0 {
		g.RateLimit.RejectLogWindow = Duration(DefaultRejectLogWindow)
	}
	defaultBucket(&g.RateLimit.PerCaller, 1.0, 20)
	defaultBucket(&g.RateLimit.Global, 5.0, 100)
	defaultBucket(&g.RateLimit.PreAuthIP, 2.0, 10)

	if c.Storage.MinFreeBytes == 0 {
		c.Storage.MinFreeBytes = DefaultMinFreeBytes
	}
	if c.GitHub.MaxInflight == 0 {
		c.GitHub.MaxInflight = DefaultMaxInflight
	}
	if c.GitHub.MaxQueueWait == 0 {
		c.GitHub.MaxQueueWait = Duration(DefaultMaxQueueWait)
	}
	if c.GitHub.BaseURL == "" {
		c.GitHub.BaseURL = DefaultGitHubBaseURL
	}
	if c.Inventory.HistoryDays == 0 {
		c.Inventory.HistoryDays = DefaultHistoryDays
	}
	if c.Inventory.RefreshInterval == 0 {
		c.Inventory.RefreshInterval = Duration(DefaultRefreshInterval)
	}
	if c.Observability.LogLevel == "" {
		c.Observability.LogLevel = DefaultLogLevel
	}
	if c.Observability.LogFormat == "" {
		c.Observability.LogFormat = DefaultLogFormat
	}
	if c.Clock.MaxDriftSeconds == 0 {
		c.Clock.MaxDriftSeconds = DefaultMaxDriftSeconds
	}
	for name, caller := range c.Callers {
		if caller.Role == "" {
			caller.Role = RoleCaller
			c.Callers[name] = caller
		}
	}
}

// canonicaliseWatchIPs rewrites observability.watch_ips into the one spelling
// the matcher can ever see, and returns a warning naming both spellings for
// every entry it had to change.
//
// # Why this exists
//
// The watch list matches EXACTLY against the normalised source_ip, and internal/httpapi normalises the observed address
// with netip.ParseAddr(...).Unmap().String() on every request. The configured
// side was stored verbatim, so `0:0:0:0:0:0:0:1`, `2001:DB8::1` and
// `::ffff:192.0.2.7` all passed `config validate` — Validate accepts anything
// ParseAddr parses — and then matched nothing, forever.
//
// That failure had no symptom at all. By design an empty watch list
// emits no series, not even a zero one, so "my entry never matches" and
// "watching is off, as designed" produced byte-identical evidence: no error,
// no log line, no series on /metrics. A silent success: the work simply never
// happens and nothing says so.
//
// # Why canonicalise rather than reject
//
// The entry is semantically fine; only its spelling is. Rejecting would refuse
// a config that has been correct all along, on a key whose whole purpose is
// diagnostic. Canonicalising means such a file keeps loading and starts doing
// what its author meant.
//
// # Why it still warns
//
// Silence would leave the operator's file saying one thing while the system
// does another. The warning is the loud half of the repair and names both
// spellings so the file can be brought into line; it is a warning and not an
// error because nothing is broken.
//
// An entry ParseAddr rejects is left exactly as the operator typed it, so
// Validate reports it as the error it is rather than this function eating it.
func (c *Config) canonicaliseWatchIPs() []string {
	var warnings []string
	for i, entry := range c.Observability.WatchIPs {
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			continue
		}
		canonical := addr.Unmap().String()
		if canonical == entry {
			continue
		}
		c.Observability.WatchIPs[i] = canonical
		warnings = append(warnings, fmt.Sprintf(
			"observability.watch_ips[%d]: %q is not the canonical spelling of that address and has been read as %q. "+
				"The observed address is always canonical, so %q on its own would have matched nothing and said nothing. "+
				"Write %q in standpipe.yaml.",
			i, entry, canonical, entry, canonical))
	}
	return warnings
}

func defaultBucket(b *Bucket, rate float64, burst int) {
	if b.RatePerSecond == 0 {
		b.RatePerSecond = rate
	}
	if b.Burst == 0 {
		b.Burst = burst
	}
}
