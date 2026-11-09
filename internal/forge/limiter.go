package forge

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/go-github/v90/github"
)

// Lane is the client's priority split. Live dispatch is bound to a caller
// request and takes priority; inventory and bootstrap are background work.
type Lane int

const (
	// LaneDispatch is a call on the trigger path. It is never paused by the
	// quota threshold and may use every in-flight slot.
	LaneDispatch Lane = iota
	// LaneInventory is background bootstrap or refresh. It pauses below the
	// quota threshold and is held back from the last in-flight slot so a
	// dispatch is not stuck behind a full sweep.
	LaneInventory
)

func (l Lane) String() string {
	if l == LaneInventory {
		return "inventory"
	}
	return "dispatch"
}

// BucketApp is the quota bucket for App JWT calls — GET /repos/../installation
// and POST /app/installations/../access_tokens. These are counted
// separately and are not affected by the rate_remaining threshold, because
// pausing token minting would pause the very calls needed to recover.
const BucketApp = "app"

// installationBucket is the quota bucket key for one installation. Quota is
// per installation, so a single global counter would pause the wrong one.
func installationBucket(installationID int64) string {
	return "installation:" + strconv.FormatInt(installationID, 10)
}

// quota is the last rate-limit state observed for one bucket.
type quota struct {
	limit      int
	remaining  int
	reset      time.Time
	observedAt time.Time
}

// limiter bounds concurrency, enforces the queue wait, and keeps per-bucket
// quota accounting.
//
// Concurrency is bounded, NOT serialized. One in-flight
// request with a 10s upstream timeout is 0.1 req/s worst case, which cannot
// serve a configured 5 req/s global caller rate; the queue would fill and
// every entry would fall to its own timeout, producing `unknown` states and
// feeding the resolver a sweep race.
type limiter struct {
	// slots is the shared bound: github.max_inflight.
	slots chan struct{}
	// backgroundSlots holds inventory work one slot short of the shared
	// bound, so a live dispatch is not queued behind a full background sweep.
	// With max_inflight 1 both are 1 and dispatch simply competes.
	backgroundSlots chan struct{}

	maxQueueWait   time.Duration
	pauseThreshold int

	mu      sync.Mutex
	buckets map[string]quota

	metrics Metrics

	// now is swappable so the pause decision can be reasoned about without a
	// wall clock. Durations elsewhere in the package use time.Since on a
	// monotonic reading, so a wall-clock step cannot distort them.
	now func() time.Time
}

func newLimiter(cfg Config, m Metrics) *limiter {
	background := cfg.MaxInflight - 1
	if background < 1 {
		background = 1
	}
	return &limiter{
		slots:           make(chan struct{}, cfg.MaxInflight),
		backgroundSlots: make(chan struct{}, background),
		maxQueueWait:    cfg.MaxQueueWait,
		pauseThreshold:  cfg.QuotaPauseThreshold,
		buckets:         make(map[string]quota),
		metrics:         m,
		now:             time.Now,
	}
}

// paused reports whether background work must stop for this bucket.
//
// The App bucket is never paused. A bucket with no observation yet is
// not paused — refusing to start before the first response would deadlock the
// bootstrap. Once the reset time passes the old reading is discarded, since
// the quota has refilled.
func (l *limiter) paused(bucket string) bool {
	if bucket == BucketApp {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	q, ok := l.buckets[bucket]
	if !ok || q.observedAt.IsZero() {
		return false
	}
	if !q.reset.IsZero() && l.now().After(q.reset) {
		return false
	}
	return q.remaining < l.pauseThreshold
}

// acquire takes an in-flight slot for one call. The returned release must be
// called exactly once.
//
// It returns an *Error of KindQuotaPaused when background work is paused for
// the bucket, and of KindQueueWaitExceeded when the wait exceeded
// github.max_queue_wait. Both are typed so the HTTP layer can produce
// 503 + Retry-After without string matching.
func (l *limiter) acquire(ctx context.Context, bucket string, lane Lane) (func(), error) {
	if lane == LaneInventory && l.paused(bucket) {
		l.metrics.IncCounter("gateway_github_quota_paused_total", map[string]string{"bucket": bucket})
		return nil, &Error{
			Kind: KindQuotaPaused,
			Err:  fmt.Errorf("bucket %s below rate_remaining threshold %d", bucket, l.pauseThreshold),
		}
	}

	timer := time.NewTimer(l.maxQueueWait)
	defer timer.Stop()

	var releaseBackground func()
	if lane == LaneInventory {
		select {
		case l.backgroundSlots <- struct{}{}:
			releaseBackground = func() { <-l.backgroundSlots }
		case <-ctx.Done():
			return nil, ctxError(ctx)
		case <-timer.C:
			return nil, l.queueTimeout(lane)
		}
	}

	select {
	case l.slots <- struct{}{}:
		return func() {
			<-l.slots
			if releaseBackground != nil {
				releaseBackground()
			}
		}, nil
	case <-ctx.Done():
		if releaseBackground != nil {
			releaseBackground()
		}
		return nil, ctxError(ctx)
	case <-timer.C:
		if releaseBackground != nil {
			releaseBackground()
		}
		return nil, l.queueTimeout(lane)
	}
}

func (l *limiter) queueTimeout(lane Lane) *Error {
	l.metrics.IncCounter("gateway_github_queue_timeout_total", map[string]string{"lane": lane.String()})
	return &Error{
		Kind:       KindQueueWaitExceeded,
		RetryAfter: l.maxQueueWait,
		Err:        fmt.Errorf("waited longer than github.max_queue_wait (%s) for an in-flight slot", l.maxQueueWait),
	}
}

func ctxError(ctx context.Context) *Error {
	if isTimeout(ctx.Err()) {
		return &Error{Kind: KindUpstreamTimeout, Err: ctx.Err()}
	}
	return &Error{Kind: KindUnreachable, Err: ctx.Err()}
}

// observe records the rate-limit headers of one response against its bucket.
// Called on every response, success or failure — a 403 carries the headers too.
func (l *limiter) observe(bucket string, resp *github.Response) {
	if resp == nil || resp.Rate.Limit == 0 {
		return
	}
	l.mu.Lock()
	l.buckets[bucket] = quota{
		limit:      resp.Rate.Limit,
		remaining:  resp.Rate.Remaining,
		reset:      resp.Rate.Reset.Time,
		observedAt: l.now(),
	}
	l.mu.Unlock()

	// The gauge is labelled per bucket. Bucket count is bounded
	// by the configured target list, so cardinality is safe.
	l.metrics.SetGauge("gateway_github_rate_remaining", float64(resp.Rate.Remaining),
		map[string]string{"bucket": bucket})
}

// RateSnapshot is the last observed quota for one bucket.
type RateSnapshot struct {
	Bucket     string
	Limit      int
	Remaining  int
	Reset      time.Time
	ObservedAt time.Time
	Paused     bool
}

// snapshot returns every bucket's last observation, for /metrics and for the
// detailed health endpoint (docs/reference/health.md).
func (l *limiter) snapshot() []RateSnapshot {
	l.mu.Lock()
	out := make([]RateSnapshot, 0, len(l.buckets))
	for bucket, q := range l.buckets {
		out = append(out, RateSnapshot{
			Bucket:     bucket,
			Limit:      q.limit,
			Remaining:  q.remaining,
			Reset:      q.reset,
			ObservedAt: q.observedAt,
		})
	}
	l.mu.Unlock()
	for i := range out {
		out[i].Paused = l.paused(out[i].Bucket)
	}
	return out
}
