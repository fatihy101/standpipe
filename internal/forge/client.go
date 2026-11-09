package forge

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-github/v90/github"
)

// Client is the single path from the gateway to GitHub.
//
// Everything it does is bounded by one limiter, authenticated by one App
// identity, and accounted per installation. It is safe for concurrent use.
type Client struct {
	cfg     Config
	lim     *limiter
	id      *identity
	store   Store
	metrics Metrics
	now     func() time.Time
	base    *http.Client

	mu    sync.Mutex
	repos map[string]*github.Client
}

// Option configures a Client.
type Option func(*Client)

// WithMetrics supplies the metrics registry. Without it, metrics are dropped.
func WithMetrics(m Metrics) Option {
	return func(c *Client) {
		if m != nil {
			c.metrics = m
		}
	}
}

// WithStore supplies durable storage. Dispatch, inventory and the resolver
// all need it; New succeeds without one so a caller can construct the client
// before storage is ready, but those methods will return an error.
func WithStore(s Store) Option {
	return func(c *Client) { c.store = s }
}

// WithHTTPClient supplies the base HTTP client. Its Transport is reused for
// every GitHub call; its Timeout is replaced by the widest per-call budget
// (httpClientTimeout), with the real deadline set per call.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.base = h }
}

// New builds a Client. It reads the App private key from disk, so it fails
// fast on a misconfigured or unreadable credential rather than at first
// dispatch.
func New(cfg Config, opts ...Option) (*Client, error) {
	// Guard on the non-negotiable: contents is read-only, always.
	if permissionContents != "read" {
		return nil, &Error{Kind: KindUnknown, Message: "contents permission must be read; the gateway never writes workflow files"}
	}

	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	c := &Client{
		cfg:     cfg,
		metrics: nopMetrics{},
		now:     time.Now,
		repos:   make(map[string]*github.Client),
	}
	for _, o := range opts {
		o(c)
	}

	c.lim = newLimiter(cfg, c.metrics)
	id, err := newIdentity(cfg, c.lim, c.metrics, c.base)
	if err != nil {
		return nil, err
	}
	c.id = id
	return c, nil
}

// RateSnapshots exposes the per-bucket quota state for /metrics and the
// detailed health endpoint (docs/reference/health.md).
func (c *Client) RateSnapshots() []RateSnapshot { return c.lim.snapshot() }

// tokenTransport authenticates each request with the current installation
// token for one repo. Fetching per request rather than per client is what
// makes the 5-minute renewal transparent to call sites.
type tokenTransport struct {
	c    *Client
	repo Repo
	base http.RoundTripper
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	token, _, err := t.c.id.token(req.Context(), t.repo)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+token)
	return t.base.RoundTrip(clone)
}

// repoClient returns the installation-authenticated client for a repo.
func (c *Client) repoClient(repo Repo) (*github.Client, error) {
	key := repo.String()
	c.mu.Lock()
	if gh, ok := c.repos[key]; ok {
		c.mu.Unlock()
		return gh, nil
	}
	c.mu.Unlock()

	httpClient := &http.Client{
		// The widest per-call budget, not the per-call budget itself: this is
		// a hard cap and per-call deadlines are set below. See
		// dispatchlatency.go.
		Timeout:   httpClientTimeout(c.cfg),
		Transport: &tokenTransport{c: c, repo: repo, base: transportOf(c.base)},
	}
	gh, err := newGitHubClient(c.cfg, httpClient)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.repos[key]; ok {
		return existing, nil
	}
	c.repos[key] = gh
	return gh, nil
}

// call runs one GitHub API call against a repo: it resolves the installation
// (for the quota bucket), takes an in-flight slot, applies the upstream
// timeout, records the rate headers, and classifies the error.
//
// There is no retry here, for any verb. A dispatch that timed out may already
// have landed, so it is never retried, and a retry helper that "only" retried GETs would be one edit away
// from retrying a POST.
func call[T any](
	ctx context.Context,
	c *Client,
	repo Repo,
	lane Lane,
	fn func(ctx context.Context, gh *github.Client) (T, *github.Response, error),
) (T, *github.Response, error) {
	return callWithTimeout(ctx, c, repo, lane, c.cfg.UpstreamTimeout, fn)
}

// callWithTimeout is call with an explicit deadline budget. It exists for one
// caller — the dispatch, which owns the DispatchLatencyModel seam. Measurement
// on 2026-08-21 confirmed LatencyIndependent, so today this hands the dispatch
// the same upstream timeout as everything else; it is kept because it is what
// makes flipping that seam a one-line change (see dispatchlatency.go).
// Everything else uses call and the single upstream timeout.
func callWithTimeout[T any](
	ctx context.Context,
	c *Client,
	repo Repo,
	lane Lane,
	timeout time.Duration,
	fn func(ctx context.Context, gh *github.Client) (T, *github.Response, error),
) (T, *github.Response, error) {
	var zero T

	instID, err := c.id.installationID(ctx, repo)
	if err != nil {
		return zero, nil, err
	}
	bucket := installationBucket(instID)

	release, err := c.lim.acquire(ctx, bucket, lane)
	if err != nil {
		return zero, nil, err
	}
	defer release()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	gh, err := c.repoClient(repo)
	if err != nil {
		return zero, nil, err
	}

	out, resp, err := fn(callCtx, gh)
	c.lim.observe(bucket, resp)

	if err != nil {
		e := classify(err, resp)
		if e.Kind == KindSecondaryRateLimit {
			// The metrics catalog labels this by installation_id, not by repo. GitHub's
			// secondary limit is applied per installation, so a repo label
			// splits one throttle across every repo that shares the
			// installation and inflates the series count for no signal. instID
			// is in hand here, which is why the divergence was a code bug and
			// not a spec impossibility.
			c.metrics.IncCounter("gateway_github_secondary_rate_limit_total",
				map[string]string{"installation_id": strconv.FormatInt(instID, 10)})
		}
		if e.Kind == KindCredentialExpired {
			// The token was rejected. Drop it so the next call mints a fresh
			// one; do not retry now, because this call may have been a POST.
			c.id.invalidate(instID, repo)
		}
		return zero, resp, e
	}
	return out, resp, nil
}
