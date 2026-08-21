package forge

import (
	"errors"
	"fmt"
	"time"
)

// Config is the subset of standpipe.yaml this package consumes.
//
// It is deliberately a plain struct owned by this package rather than a type
// borrowed from internal/config: package forge does not import the config
// package, so the two can be developed independently. The caller maps the
// parsed YAML onto this struct.
type Config struct {
	// AppID is github.app_id.
	AppID int64
	// PrivateKeyPath is github.private_key_path — a PEM-encoded RSA key.
	PrivateKeyPath string

	// MaxInflight is github.max_inflight (default 4): the upper bound on
	// concurrent GitHub API calls. It is NOT 1. A
	// serialized client contradicts the configured caller rate: one flight
	// with a 10s upstream timeout is 0.1 req/s worst case, while the global
	// caller limit is 5 req/s with a burst of 100.
	MaxInflight int
	// MaxQueueWait is github.max_queue_wait (default 2s). Exceeding it yields
	// ErrQueueWaitExceeded, which the HTTP layer maps to 503 + Retry-After.
	MaxQueueWait time.Duration
	// UpstreamTimeout is gateway.upstream_timeout (default 10s).
	//
	// Do NOT lower it. Measured 2026-08-21: return_run_details adds no
	// latency, but ~1.4 s is the BASE cost of a dispatch on an IDLE account —
	// so what a shorter timeout would eat is GitHub's and the network's own
	// floor, not tier 1's overhead, and the sample says nothing about a loaded
	// installation. See dispatchlatency.go.
	UpstreamTimeout time.Duration

	// ReturnRunDetails is github.return_run_details (default TRUE): ask
	// the dispatch endpoint for the created run's id, which is correlation
	// tier 1.
	//
	// It is a *bool and not a bool because the default is true: an unset field
	// must mean "default" and not "off". nil is normalised to true by
	// withDefaults; read it through tier1Armed, never by dereferencing.
	//
	// Turning it off is an escape hatch, not a tuning knob: the only legitimate reason is an API that rejects the parameter —
	// GHES, or a GitHub regression. Off means correlation falls entirely to
	// tiers 2 and 3, and it is the ONLY way the correlation value "window" can
	// ever be recorded.
	ReturnRunDetails *bool

	// QuotaPauseThreshold is the quota pause threshold. Below it, inventory
	// and bootstrap traffic pauses *for that installation*; live dispatch
	// continues. App JWT calls sit in their own bucket and ignore it.
	QuotaPauseThreshold int

	// ResolverInterval is how often the correlation resolver sweeps (default 15s).
	// The interval is only a wake-up; the resolver's state lives in the DB.
	ResolverInterval time.Duration
	// ResolverHorizon is how long an unmatched trigger stays resolvable
	// before it is written as resolution "none" (default 5m).
	ResolverHorizon time.Duration
	// ResolverLookback widens the run-listing window around the dispatch
	// timestamp (default 2m: the run listing filters on created>=dispatch_ts-2m).
	ResolverLookback time.Duration
	// ResolverBatch bounds one sweep (default 200).
	ResolverBatch int

	// WorkflowCacheTTL is the stale-while-revalidate TTL (default 1h).
	// An entry past its TTL is stale-but-usable, never discarded. Only a
	// never-fetched workflow is cold.
	WorkflowCacheTTL time.Duration
	// InstallationTTL is the installation-lookup cache TTL (default 1h).
	InstallationTTL time.Duration
	// TokenRefreshLead is how far before expiry an installation token is
	// renewed (default 5m).
	TokenRefreshLead time.Duration

	// InventoryHistoryDays is inventory.history_days (default 30).
	InventoryHistoryDays int
	// InventoryRefreshInterval is inventory.refresh_interval (default 6h).
	InventoryRefreshInterval time.Duration

	// BaseURL overrides the GitHub API root. Empty means github.com. Present
	// so the package can be pointed at a recorded fixture server without a
	// build tag.
	BaseURL string

	// UserAgent is sent on every request.
	UserAgent string
}

// Defaults.
const (
	DefaultMaxInflight              = 4
	DefaultMaxQueueWait             = 2 * time.Second
	DefaultUpstreamTimeout          = 10 * time.Second
	DefaultReturnRunDetails         = true
	DefaultQuotaPauseThreshold      = 500
	DefaultResolverInterval         = 15 * time.Second
	DefaultResolverHorizon          = 5 * time.Minute
	DefaultResolverLookback         = 2 * time.Minute
	DefaultResolverBatch            = 200
	DefaultWorkflowCacheTTL         = time.Hour
	DefaultInstallationTTL          = time.Hour
	DefaultTokenRefreshLead         = 5 * time.Minute
	DefaultInventoryHistoryDays     = 30
	DefaultInventoryRefreshInterval = 6 * time.Hour
	DefaultUserAgent                = "standpipe-gateway"
)

// withDefaults fills unset fields. It never overwrites a set value.
func (c Config) withDefaults() Config {
	if c.MaxInflight <= 0 {
		c.MaxInflight = DefaultMaxInflight
	}
	if c.MaxQueueWait <= 0 {
		c.MaxQueueWait = DefaultMaxQueueWait
	}
	if c.UpstreamTimeout <= 0 {
		c.UpstreamTimeout = DefaultUpstreamTimeout
	}
	if c.ReturnRunDetails == nil {
		v := DefaultReturnRunDetails
		c.ReturnRunDetails = &v
	}
	if c.QuotaPauseThreshold <= 0 {
		c.QuotaPauseThreshold = DefaultQuotaPauseThreshold
	}
	if c.ResolverInterval <= 0 {
		c.ResolverInterval = DefaultResolverInterval
	}
	if c.ResolverHorizon <= 0 {
		c.ResolverHorizon = DefaultResolverHorizon
	}
	if c.ResolverLookback <= 0 {
		c.ResolverLookback = DefaultResolverLookback
	}
	if c.ResolverBatch <= 0 {
		c.ResolverBatch = DefaultResolverBatch
	}
	if c.WorkflowCacheTTL <= 0 {
		c.WorkflowCacheTTL = DefaultWorkflowCacheTTL
	}
	if c.InstallationTTL <= 0 {
		c.InstallationTTL = DefaultInstallationTTL
	}
	if c.TokenRefreshLead <= 0 {
		c.TokenRefreshLead = DefaultTokenRefreshLead
	}
	if c.InventoryHistoryDays <= 0 {
		c.InventoryHistoryDays = DefaultInventoryHistoryDays
	}
	if c.InventoryRefreshInterval <= 0 {
		c.InventoryRefreshInterval = DefaultInventoryRefreshInterval
	}
	if c.UserAgent == "" {
		c.UserAgent = DefaultUserAgent
	}
	return c
}

// tier1Armed reports whether this deployment asks GitHub for run details, i.e.
// whether correlation tier 1 is armed. An unset value means armed — a config file that
// never mentions return_run_details gets the documented default.
func (c Config) tier1Armed() bool {
	return c.ReturnRunDetails == nil || *c.ReturnRunDetails
}

// validate reports configuration that cannot produce a working client.
func (c Config) validate() error {
	var errs []error
	if c.AppID <= 0 {
		errs = append(errs, errors.New("github.app_id is required and must be positive"))
	}
	if c.PrivateKeyPath == "" {
		errs = append(errs, errors.New("github.private_key_path is required"))
	}
	if c.MaxInflight < 1 {
		errs = append(errs, fmt.Errorf("github.max_inflight must be >= 1, got %d", c.MaxInflight))
	}
	return errors.Join(errs...)
}
