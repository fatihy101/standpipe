package testutil

import (
	"sync"
	"time"
)

// Clock is a settable wall clock for tests. It satisfies the shape that
// store.Config.Now expects (func() time.Time) via its Now method.
//
// Every method is safe for concurrent use: the concurrency tests read the
// clock from N goroutines at once and a plain field would be a data race that
// -race would (correctly) report as a test bug rather than a product bug.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a Clock pinned at t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Set moves the clock to t, forwards or backwards.
func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// Advance moves the clock forward by d. A negative d moves it backwards, which
// is a case the store's wall-clock TTL has to survive.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// BaseTime is a fixed, readable instant used as the default start of test
// timelines. It is deliberately far from any DST or leap-second boundary.
var BaseTime = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
