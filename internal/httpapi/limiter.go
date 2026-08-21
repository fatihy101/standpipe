package httpapi

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/fatihy101/standpipe/internal/config"
)

// ---------------------------------------------------------------------------
// INBOUND rate limiting.
//
// Everything in this file limits CALLERS talking to the gateway. It shares
// nothing with internal/forge/limiter.go, which limits the gateway talking to
// GitHub, and forge.KindPrimaryRateLimit / KindSecondaryRateLimit are upstream
// GitHub errors that never appear here. Two limiters with
// the same name in one process is how a "you are going too fast" and a "GitHub
// is throttling us" end up indistinguishable to a caller.
// ---------------------------------------------------------------------------

// The pre-auth store bounds. They are NOT configuration: the YAML schema does not
// carry them, and adding keys the design does not define would put this package's
// invention into the operator's file. Fixed here.
const (
	preAuthMaxEntries = 10000
	preAuthTTL        = 10 * time.Minute
)

// ipLimiter is step 6: the pre-authentication, per-IP limiter.
//
// It is a FIXED-CAPACITY LRU on purpose. A map that grew
// with the source addresses it saw would let unauthenticated traffic consume
// memory, which is the same failure the limiter exists to prevent one layer
// down. Nothing here touches the database: a request refused before
// authentication cannot be attributed to a caller, so its only record is a
// counter (trigger_rejected explicitly puts step 6 out of scope).
type ipLimiter struct {
	mu       sync.Mutex
	entries  map[string]*list.Element
	order    *list.List // front = most recently used
	limit    rate.Limit
	burst    int
	capacity int
	ttl      time.Duration
	now      func() time.Time
}

type ipEntry struct {
	key  string
	lim  *rate.Limiter
	seen time.Time
}

func newIPLimiter(b config.Bucket, now func() time.Time) *ipLimiter {
	if now == nil {
		now = time.Now
	}
	return &ipLimiter{
		entries:  make(map[string]*list.Element),
		order:    list.New(),
		limit:    rate.Limit(b.RatePerSecond),
		burst:    b.Burst,
		capacity: preAuthMaxEntries,
		ttl:      preAuthTTL,
		now:      now,
	}
}

// allow reports whether the address may proceed, and how long it should wait
// if not.
func (l *ipLimiter) allow(ip string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if el, ok := l.entries[ip]; ok {
		e := el.Value.(*ipEntry)
		e.seen = now
		l.order.MoveToFront(el)
		return reserve(e.lim, now)
	}

	l.evictLocked(now)
	e := &ipEntry{key: ip, lim: rate.NewLimiter(l.limit, l.burst), seen: now}
	l.entries[ip] = l.order.PushFront(e)
	return reserve(e.lim, now)
}

// evictLocked drops expired entries, then the least recently used ones until
// there is room. A caller that is being limited stays hot and therefore stays
// in the map; an attacker rotating source addresses evicts only themselves.
func (l *ipLimiter) evictLocked(now time.Time) {
	for el := l.order.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*ipEntry)
		if now.Sub(e.seen) < l.ttl {
			break
		}
		l.order.Remove(el)
		delete(l.entries, e.key)
		el = prev
	}
	for len(l.entries) >= l.capacity {
		el := l.order.Back()
		if el == nil {
			return
		}
		e := el.Value.(*ipEntry)
		l.order.Remove(el)
		delete(l.entries, e.key)
	}
}

// Len is the current entry count, for the admin health surface.
func (l *ipLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// reserve takes one token without ever borrowing from the future. Delay is
// returned so Retry-After carries the real wait rather than a guess.
func reserve(l *rate.Limiter, now time.Time) (bool, time.Duration) {
	r := l.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d
	}
	return true, 0
}

// callerLimiters is step 8: the per-caller limiter plus the global one.
//
// A generation is rebuilt wholesale when gateway.rate_limit changes on SIGHUP
// reload. Rebuilding resets every bucket to full, which is a deliberate
// choice: carrying tokens across a rate change would mean the new burst is not
// what the operator configured until the old one drains, and the operator's
// reason for the SIGHUP is usually that the current limit is wrong.
type callerLimiters struct {
	mu       sync.Mutex
	perCall  map[string]*rate.Limiter
	global   *rate.Limiter
	rate     rate.Limit
	burst    int
	now      func() time.Time
	settings config.RateLimit
}

func newCallerLimiters(rl config.RateLimit, now func() time.Time) *callerLimiters {
	if now == nil {
		now = time.Now
	}
	return &callerLimiters{
		perCall:  make(map[string]*rate.Limiter),
		global:   rate.NewLimiter(rate.Limit(rl.Global.RatePerSecond), rl.Global.Burst),
		rate:     rate.Limit(rl.PerCaller.RatePerSecond),
		burst:    rl.PerCaller.Burst,
		now:      now,
		settings: rl,
	}
}

// globalTicket is a reservation held against the global bucket.
//
// Step 8: replay requests do not consume the global limit. Whether a request is
// a replay is not known until the reservation at step 14, six steps later. So
// the token is taken here and RETURNED at step 14 if the reservation turns out
// to be a replay — which is what x/time/rate's Reservation.Cancel is for. The
// alternative, checking idempotency before the limiter, would move a database
// read in front of the rate limit and hand an unauthenticated-shaped attacker
// a free query.
type globalTicket struct {
	res *rate.Reservation
	at  time.Time
}

// Refund returns the token to the global bucket. Safe on a zero value.
func (t *globalTicket) Refund() {
	if t == nil || t.res == nil {
		return
	}
	t.res.CancelAt(t.at)
	t.res = nil
}

// allow applies the per-caller limit and then the global one.
//
// Order matters: a caller over its own limit must not consume a global token
// on the way to being refused, or one noisy caller drains the budget of every
// other caller by being rejected.
func (c *callerLimiters) allow(caller string) (ok bool, retry time.Duration, ticket *globalTicket) {
	now := c.now()

	c.mu.Lock()
	lim, exists := c.perCall[caller]
	if !exists {
		lim = rate.NewLimiter(c.rate, c.burst)
		c.perCall[caller] = lim
	}
	global := c.global
	c.mu.Unlock()

	if ok, d := reserve(lim, now); !ok {
		return false, d, nil
	}

	r := global.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second, nil
	}
	if d := r.DelayFrom(now); d > 0 {
		r.CancelAt(now)
		return false, d, nil
	}
	return true, 0, &globalTicket{res: r, at: now}
}

// Forget drops a caller's bucket. Used when a token is revoked on SIGHUP so a
// deleted caller does not hold memory forever.
func (c *callerLimiters) Forget(known map[string]config.Caller) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name := range c.perCall {
		if _, ok := known[name]; !ok {
			delete(c.perCall, name)
		}
	}
}
