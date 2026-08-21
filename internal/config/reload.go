package config

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

// Reloader holds the live configuration and applies the SIGHUP reload semantics.
//
// # The question this type exists to answer
//
// "If `config validate` fails during a reload, is the old configuration
// preserved?" Yes, explicitly: *if `config validate` fails, the old
// configuration is preserved, the error is logged and the gateway keeps
// running. SIGHUP must never be a way to shoot yourself in the foot.*
//
// That is enforced structurally here rather than by discipline at the call
// site: Reload parses and validates into a NEW value and only then publishes
// it. Nothing mutates the live configuration in place, so there is no code
// path on which a half-parsed file can be observed, and a failing reload can
// only return an error.
//
// # In-flight requests
//
// Current returns an immutable snapshot. A request handler calls it ONCE, at
// the top, and uses that pointer for its whole lifetime (in-flight requests
// complete under the OLD configuration). A reload swaps the pointer;
// a request already holding the previous one never sees a mixed policy, so
// there is no window in which a caller is authenticated under one allowlist
// and authorized under another.
//
// # What reloads
//
// `callers`, `allowed_targets` and `rate_limit` only. `github` and
// `storage` need a restart. A change to a non-reloadable field is NOT applied
// and NOT silently dropped either — it is returned in Result.Ignored so the
// caller logs it. An operator who edits `storage.sqlite_path`, sends SIGHUP
// and sees nothing in the log would reasonably believe the gateway moved.
type Reloader struct {
	path string

	cur atomic.Pointer[Config]

	// mu serialises reloads. Reads go through the atomic pointer and never
	// take it.
	mu sync.Mutex
}

// NewReloader publishes an already-validated configuration.
func NewReloader(cfg *Config) *Reloader {
	r := &Reloader{path: cfg.Path}
	r.cur.Store(cfg)
	return r
}

// Current is the live configuration. Treat the result as immutable.
func (r *Reloader) Current() *Config { return r.cur.Load() }

// Result describes what a successful reload did.
type Result struct {
	// Config is the newly published configuration.
	Config *Config
	// Ignored names the non-reloadable fields that changed on disk and were
	// NOT applied. Empty is the normal case.
	Ignored []string
	// Warnings are the parse warnings of the new file (known-future blocks).
	Warnings []string
	// RateLimitChanged reports whether the limiter buckets must be rebuilt.
	RateLimitChanged bool
	// CallersChanged reports whether the token table changed, i.e. whether a
	// token was revoked or added.
	CallersChanged bool
	// TargetsChanged reports whether allowed_targets changed.
	TargetsChanged bool
}

// Reload re-reads the file. On ANY error the live configuration is untouched
// and the error is returned for the caller to log at error level; the gateway
// keeps running with what it had.
func (r *Reloader) Reload() (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	old := r.cur.Load()
	fresh, err := Load(r.path)
	if err != nil {
		// The old configuration is preserved. Nothing has been
		// published, because publication is the last statement of this
		// function and it is unreachable from here.
		return Result{}, err
	}

	next := *old
	next.Warnings = fresh.Warnings
	res := Result{Warnings: fresh.Warnings}

	// The three reloadable blocks.
	if !reflect.DeepEqual(old.Callers, fresh.Callers) {
		next.Callers = fresh.Callers
		res.CallersChanged = true
	}
	if !reflect.DeepEqual(old.AllowedTargets, fresh.AllowedTargets) {
		next.AllowedTargets = fresh.AllowedTargets
		res.TargetsChanged = true
	}
	if !reflect.DeepEqual(old.Gateway.RateLimit, fresh.Gateway.RateLimit) {
		next.Gateway.RateLimit = fresh.Gateway.RateLimit
		res.RateLimitChanged = true
	}

	// Everything else: report the difference, apply nothing.
	res.Ignored = ignoredChanges(old, fresh)

	r.cur.Store(&next)
	res.Config = &next
	return res, nil
}

// ignoredChanges names every non-reloadable field that differs. It is a
// hand-written list, and a new field added to Config without a line here is
// invisible on reload — which is why the list is grouped exactly like the
// configuration YAML and why the block-level fallback at the end exists.
func ignoredChanges(old, fresh *Config) []string {
	var out []string
	add := func(name string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			out = append(out, fmt.Sprintf("%s (restart required)", name))
		}
	}
	add("gateway.listen", old.Gateway.Listen, fresh.Gateway.Listen)
	add("gateway.admin_listen", old.Gateway.AdminListen, fresh.Gateway.AdminListen)
	add("gateway.trusted_proxies", old.Gateway.TrustedProxies, fresh.Gateway.TrustedProxies)
	add("gateway.idempotency_ttl", old.Gateway.IdempotencyTTL, fresh.Gateway.IdempotencyTTL)
	add("gateway.max_body_bytes", old.Gateway.MaxBodyBytes, fresh.Gateway.MaxBodyBytes)
	add("gateway.upstream_timeout", old.Gateway.UpstreamTimeout, fresh.Gateway.UpstreamTimeout)
	add("gateway.shutdown_grace", old.Gateway.ShutdownGrace, fresh.Gateway.ShutdownGrace)
	add("gateway.retention_days", old.Gateway.RetentionDays, fresh.Gateway.RetentionDays)
	add("storage", old.Storage, fresh.Storage)
	add("github", old.GitHub, fresh.GitHub)
	add("inventory", old.Inventory, fresh.Inventory)
	add("observability", old.Observability, fresh.Observability)
	add("clock", old.Clock, fresh.Clock)
	return out
}
