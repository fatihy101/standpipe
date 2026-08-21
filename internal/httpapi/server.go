// Package httpapi is the gateway's HTTP surface.
//
// It owns the public trigger listener and the separate admin listener, the
// request-processing order, the error table, and the INBOUND rate limiters.
// It writes no SQL and never talks to GitHub directly: durable storage is the
// Store interface, GitHub is the Dispatcher interface.
//
// Two boundaries are worth stating up front because breaking them is silent:
//
//   - The inbound limiters here have nothing to do with
//     internal/forge/limiter.go, and forge's rate-limit error Kinds are
//     UPSTREAM GitHub errors that must never be reused for a caller 429.
//   - repo/workflow/caller reach a metric label only after authentication and
//     allowlist validation, and there is no source_ip label anywhere.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
)

// Options builds a Server.
type Options struct {
	Reloader *config.Reloader
	Store    Store
	Forge    Dispatcher
	Metrics  *metrics.Registry
	Log      *slog.Logger
	Version  string

	// Now, NewEventID and NewRequestID are injection points for tests.
	Now          func() time.Time
	NewEventID   func() string
	NewRequestID func() string
}

// Server is the HTTP surface.
type Server struct {
	reloader *config.Reloader
	store    Store
	forge    Dispatcher
	metrics  *metrics.Registry
	log      *slog.Logger
	version  string

	now          func() time.Time
	newEventID   func() string
	newRequestID func() string

	trustedProxies []netip.Prefix
	ipLimit        *ipLimiter
	// callerLim is swapped wholesale when gateway.rate_limit changes on
	// SIGHUP. It is atomic so a request in flight keeps the limiter it took a
	// ticket from and cannot refund into a bucket that no longer exists.
	callerLim atomic.Pointer[callerLimiters]
	rejects   *rejectLogger

	startedAt time.Time
}

// New builds the server from an already-validated configuration.
func New(opts Options) (*Server, error) {
	if opts.Reloader == nil {
		return nil, errors.New("httpapi: a config.Reloader is required")
	}
	if opts.Store == nil {
		return nil, errors.New("httpapi: a Store is required")
	}
	if opts.Forge == nil {
		return nil, errors.New("httpapi: a Dispatcher is required")
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewEventID == nil {
		opts.NewEventID = store.NewEventID
	}
	if opts.NewRequestID == nil {
		opts.NewRequestID = store.NewRequestID
	}

	cfg := opts.Reloader.Current()
	s := &Server{
		reloader:       opts.Reloader,
		store:          opts.Store,
		forge:          opts.Forge,
		metrics:        opts.Metrics,
		log:            opts.Log,
		version:        opts.Version,
		now:            opts.Now,
		newEventID:     opts.NewEventID,
		newRequestID:   opts.NewRequestID,
		trustedProxies: parsePrefixes(cfg.Gateway.TrustedProxies),
		ipLimit:        newIPLimiter(cfg.Gateway.RateLimit.PreAuthIP, opts.Now),
		startedAt:      opts.Now(),
	}
	s.callerLim.Store(newCallerLimiters(cfg.Gateway.RateLimit, opts.Now))
	s.rejects = newRejectLogger(opts.Store, opts.Metrics, opts.Log,
		cfg.Gateway.RateLimit.RejectLogWindow.D(), opts.Now)

	s.metrics.SetWatchIPs(cfg.Observability.WatchIPs)
	s.metrics.OnCollect(s.collect)
	return s, nil
}

func (s *Server) callers() *callerLimiters { return s.callerLim.Load() }

// ApplyConfig adopts what a SIGHUP reload changed.
//
// It is called with the Result of config.Reloader.Reload, i.e. only after the
// new file parsed AND validated. A failed reload never reaches here: the old
// configuration stays live and the gateway keeps running, because SIGHUP must
// never be a way to shoot the process in the foot.
func (s *Server) ApplyConfig(res config.Result) {
	if res.Config == nil {
		return
	}
	if res.RateLimitChanged {
		// Rebuilt wholesale. The buckets restart full: carrying tokens across
		// a rate change would mean the operator's new burst is not in effect
		// until the old one drains, and the reason for the SIGHUP is usually
		// that the current one is wrong.
		s.callerLim.Store(newCallerLimiters(res.Config.Gateway.RateLimit, s.now))
		// reject_log_window lives under rate_limit and reloads with it.
		s.rejects.SetWindow(res.Config.Gateway.RateLimit.RejectLogWindow.D())
		s.log.Info("rate limits reloaded",
			"reject_log_window", res.Config.Gateway.RateLimit.RejectLogWindow.D().String())
	}
	if res.CallersChanged {
		// A revoked caller's limiter is dropped so a deleted caller does
		// not hold memory for the life of the process.
		s.callers().Forget(res.Config.Callers)
		s.log.Info("caller table reloaded", "callers", len(res.Config.Callers))
	}
	if res.TargetsChanged {
		s.log.Info("allowed_targets reloaded", "targets", len(res.Config.AllowedTargets))
	}
	s.metrics.SetWatchIPs(res.Config.Observability.WatchIPs)
	for _, ignored := range res.Ignored {
		s.log.Warn("configuration change NOT applied", "field", ignored)
	}
	for _, w := range res.Warnings {
		s.log.Warn("configuration warning", "detail", w)
	}
}

// Close flushes the coalesced rejection windows.
//
// The design accepts losing an open window's counter on a CRASH and says so
// explicitly. Extending that to an orderly shutdown would turn a documented
// residual risk into an undocumented one, so the windows are flushed here.
func (s *Server) Close() error {
	s.rejects.Close()
	return nil
}

// PublicHandler is the internet-facing surface (the trigger endpoint and the
// public /healthz).
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()

	// The method is NOT in the pattern. Registering "POST /t/..." would make
	// a GET fall through to the catch-all, and the contract requires 405 for a
	// non-POST method on this path.
	mux.HandleFunc("/t/{owner}/{repo}/{workflow}", s.handleTrigger)
	mux.HandleFunc("/healthz", s.handlePublicHealth)

	// There is NO 404 on this surface. An unconfigured path answers 403,
	// identically to an unauthorized one, so responses cannot be used to
	// enumerate what exists.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r, s.newRequestID)
		w.Header().Set("X-Request-Id", id)
		writeError(w, id, errForbidden(DetailUnknownPath))
	})
	return mux
}

// AdminHandler is the separate admin listener: /metrics, a detailed /healthz and
// /inventory. It binds to its own address and is never mounted on the public
// mux.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", s.metrics.Handler())
	mux.HandleFunc("/healthz", s.handleAdminHealth)
	mux.HandleFunc("/inventory", s.handleInventory)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		id := requestID(r, s.newRequestID)
		w.Header().Set("X-Request-Id", id)
		writeError(w, id, errForbidden(DetailUnknownPath))
	})
	return mux
}

// handlePublicHealth is the public /healthz. It answers 200 whenever the gateway can accept
// requests, EVEN IF GitHub is unreachable — a gateway that reports itself
// unhealthy during a GitHub outage would be taken out of rotation exactly when
// it is needed.
func (s *Server) handlePublicHealth(w http.ResponseWriter, r *http.Request) {
	id := requestID(r, s.newRequestID)
	w.Header().Set("X-Request-Id", id)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, id, errMethodNotAllowed())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.HealthCheck(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// adminHealth is the admin /healthz's detailed body.
type adminHealth struct {
	Status             string   `json:"status"`
	Version            string   `json:"version"`
	UptimeSeconds      int64    `json:"uptime_seconds"`
	SQLiteOK           bool     `json:"sqlite_ok"`
	SchemaVersion      int      `json:"schema_version"`
	GitHubReachable    bool     `json:"github_reachable"`
	GitHubRateRemain   int      `json:"github_rate_remaining"`
	GitHubRateResetAt  int64    `json:"github_rate_reset_at"`
	PreAuthLimiterSize int      `json:"pre_auth_limiter_entries"`
	RejectWindowsOpen  int      `json:"reject_windows_open"`
	MetricsDrift       []string `json:"metrics_drift,omitempty"`
}

func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	h := adminHealth{
		Status:             "ok",
		Version:            s.version,
		UptimeSeconds:      int64(s.now().Sub(s.startedAt).Seconds()),
		PreAuthLimiterSize: s.ipLimit.Len(),
		RejectWindowsOpen:  s.rejects.Pending(),
		// Surfaced so the metrics catalog drifting away from what the code
		// emits is visible without diffing the reference by hand.
		MetricsDrift: s.metrics.Drift(),
	}

	h.SQLiteOK = s.store.HealthCheck(ctx) == nil
	// Free space below the threshold reports sqlite_ok: false, because
	// trigger requests are already answering 503 storage_degraded.
	if err := s.store.CheckFreeSpace(); err != nil {
		h.SQLiteOK = false
	}
	if v, err := s.store.SchemaVersion(ctx); err == nil {
		h.SchemaVersion = v
	}
	if !h.SQLiteOK {
		h.Status = "degraded"
	}

	for _, snap := range s.forge.RateSnapshots() {
		if snap.ObservedAt.IsZero() {
			continue
		}
		h.GitHubReachable = true
		if h.GitHubRateRemain == 0 || snap.Remaining < h.GitHubRateRemain {
			h.GitHubRateRemain = snap.Remaining
			h.GitHubRateResetAt = snap.Reset.Unix()
		}
	}

	status := http.StatusOK
	if !h.SQLiteOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, h)
}

// handleInventory is GET /inventory. It is the ONE admin endpoint that requires a
// token, and only the `admin` role.
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	id := requestID(r, s.newRequestID)
	w.Header().Set("X-Request-Id", id)
	if r.Method != http.MethodGet {
		writeError(w, id, errMethodNotAllowed())
		return
	}
	cfg := s.reloader.Current()
	caller, ok := authenticate(cfg, r.Header.Get("Authorization"))
	if !ok {
		s.metrics.IncCounter("gateway_auth_failures_total", nil)
		writeError(w, id, errUnauthorized())
		return
	}
	if !caller.IsAdmin() {
		// 403, not 404 — the surface has no 404.
		writeError(w, id, errForbidden(DetailCallerPolicy))
		return
	}

	body, err := s.Inventory(r.Context(), cfg.Inventory.HistoryDays)
	if err != nil {
		s.log.Error("inventory read failed", "err", err, "request_id", id)
		writeError(w, id, errInternal("inventory unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// Inventory builds the /inventory body. The endpoint and `standpipe inventory` share
// BuildInventory so the two cannot disagree about the coverage gap — the one
// number Phase 0's exit condition is stated in terms of.
//
// routing.json is NOT consulted here. Its validation belongs to the
// `standpipe inventory` CLI, and the gateway process never reads the file at
// all: the parser lives in package main, which internal/httpapi cannot import.
func (s *Server) Inventory(ctx context.Context, historyDays int) (*InventoryReport, error) {
	return BuildInventory(ctx, s.store, historyDays, s.now())
}

// collect refreshes the gauges whose truth lives outside the registry. It runs
// at scrape time so /metrics never publishes a value staler than the scrape.
func (s *Server) collect() {
	s.metrics.SetGauge("gateway_uptime_seconds", s.now().Sub(s.startedAt).Seconds(), nil)
	for _, snap := range s.forge.RateSnapshots() {
		labels := map[string]string{"bucket": snap.Bucket}
		s.metrics.SetGauge("gateway_github_rate_remaining", float64(snap.Remaining), labels)
		if !snap.Reset.IsZero() {
			s.metrics.SetGauge("gateway_github_rate_reset_timestamp", float64(snap.Reset.Unix()), labels)
		}
	}
}

// PublishCoverage sets the two coverage gauges. The inventory refresh
// calls it; they are gauges over a query, not counters, so nothing else may
// increment them.
func (s *Server) PublishCoverage(summary store.CoverageSummary) {
	s.metrics.SetGauge("gateway_inventory_coverage_gap_pct", summary.GapPct, nil)
	s.metrics.SetGauge("gateway_inventory_coverage_unknown_pct", summary.UnknownPct, nil)
}
