package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatihy101/standpipe/internal/metrics"
	"github.com/fatihy101/standpipe/internal/store"
)

// rejectLogger writes the `trigger_rejected` events.
//
// Two populations, deliberately handled differently:
//
//   - Steps 9-13 (policy, allowlist, bad body, unsupported inputs, secret) get
//     ONE event each, written immediately in its own transaction.
//   - Step 8 (429) is COALESCED: one event per caller per
//     rate_limit.reject_log_window, carrying suppressed_count and
//     suppressed_since. The reason — a row per refusal turns the rate
//     limiter into a write amplifier against single-writer SQLite, which is
//     the exact problem the pre-auth limiter exists to avoid, moved one layer
//     inward.
//
// Steps 6 and 7 are NOT here at all. An unauthenticated request cannot be
// attributed to a caller, so it never reaches durable storage.
type rejectLogger struct {
	store   RejectStore
	metrics *metrics.Registry
	log     *slog.Logger
	now     func() time.Time

	// window is rate_limit.reject_log_window, which SIGHUP reloads with the
	// rest of rate_limit. It is atomic because SetWindow runs on the SIGHUP
	// goroutine while the flush loop reads it.
	window atomic.Int64

	mu      sync.Mutex
	pending map[string]*rejectWindow

	stop chan struct{}
	done chan struct{}
}

// RejectStore is the write side rejectLogger needs.
type RejectStore interface {
	AppendTriggerRejected(ctx context.Context, ev store.TriggerRejectedEvent) (string, error)
}

type rejectWindow struct {
	ev    store.TriggerRejectedEvent
	count int
	since time.Time
	mixed bool
}

func newRejectLogger(s RejectStore, m *metrics.Registry, log *slog.Logger, window time.Duration, now func() time.Time) *rejectLogger {
	if now == nil {
		now = time.Now
	}
	r := &rejectLogger{
		store:   s,
		metrics: m,
		log:     log,
		now:     now,
		pending: make(map[string]*rejectWindow),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	r.SetWindow(window)
	go r.loop()
	return r
}

// SetWindow adopts a reloaded rate_limit.reject_log_window.
//
// Without this the coalescing window would keep the value it had at startup
// while gateway_trigger_rejected_total kept using the new one, and the
// difference between the counter and the events — which is defined as the
// number of coalesced requests — would quietly stop meaning that.
func (r *rejectLogger) SetWindow(d time.Duration) {
	if d <= 0 {
		d = time.Minute
	}
	r.window.Store(int64(d))
}

func (r *rejectLogger) currentWindow() time.Duration { return time.Duration(r.window.Load()) }

func (r *rejectLogger) loop() {
	defer close(r.done)
	// A quarter of the shortest window we would accept: the flush
	// granularity, not the window itself. Ticking exactly at the window would
	// put every event up to one whole window late.
	tick := r.currentWindow() / 4
	if tick < 250*time.Millisecond {
		tick = 250 * time.Millisecond
	}
	if tick > 5*time.Second {
		tick = 5 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-t.C:
			r.flushDue(false)
		}
	}
}

// Close flushes every open window and stops the loop.
//
// A graceful shutdown must not lose the counters: the design accepts losing them on
// a CRASH and says so, and quietly extending that to an orderly stop would
// turn a documented residual risk into an undocumented one.
func (r *rejectLogger) Close() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
	r.flushDue(true)
}

// Write records a non-coalesced rejection (steps 9-13).
//
// A failed write does NOT become a 500. The "no dispatch without a
// record" rule is about dispatches; nothing went to GitHub on a reject path,
// so the request is still refused and the loss is surfaced by
// gateway_reject_log_dropped_total instead of being hidden.
func (r *rejectLogger) Write(ctx context.Context, ev store.TriggerRejectedEvent) {
	if ev.TS.IsZero() {
		ev.TS = r.now()
	}
	start := r.now()
	if _, err := r.store.AppendTriggerRejected(ctx, ev); err != nil {
		r.metrics.IncCounter("gateway_reject_log_dropped_total", nil)
		r.log.Error("trigger_rejected event not written",
			"caller_name", ev.CallerName, "reject_reason", string(ev.RejectReason), "err", err)
		return
	}
	r.metrics.ObserveDuration("gateway_sqlite_write_duration_seconds", r.now().Sub(start),
		map[string]string{"table": "trigger_events"})
}

// Coalesce records a 429 into the caller's open window.
//
// A coalesced row carries NO reason_text, ever. The row stands for N requests
// and N callers' free-text reasons cannot be one string: keeping the first
// would make the field read as representative of a window it describes only by
// accident, and an operator reading `suppressed_count: 40` next to one
// sentence will take that sentence as the account of all forty. There is no
// honest single value, so there is none — the honest statement about the
// window is `suppressed_count`, which is already there.
//
// The clear is unconditional and stays unconditional. It is currently also
// unreached: reqCtx admits the reason immediately AFTER step 8 allows the
// request, so a coalesced 429 never carries one to begin with. That is a
// property of one line's placement in handleTrigger; this one is a property of
// every 429 row there will ever be, and it is the one that must not depend on
// the other. Do not delete it as dead code.
func (r *rejectLogger) Coalesce(ev store.TriggerRejectedEvent) {
	ev.ReasonText = ""
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	w, ok := r.pending[ev.CallerName]
	if !ok {
		r.pending[ev.CallerName] = &rejectWindow{ev: ev, count: 1, since: now}
		return
	}
	w.count++
	if w.ev.TargetProject != ev.TargetProject || w.ev.TargetWorkflow != ev.TargetWorkflow {
		// The target comes from the FIRST rejection; when the window
		// covers more than one target, target_workflow is written NULL and
		// the row reads as a caller-wide limit together with
		// suppressed_count > 1.
		w.mixed = true
	}
}

// flushDue writes every window that has closed. force ignores the age.
func (r *rejectLogger) flushDue(force bool) {
	now := r.now()
	window := r.currentWindow()
	r.mu.Lock()
	var ready []*rejectWindow
	for caller, w := range r.pending {
		if force || now.Sub(w.since) >= window {
			ready = append(ready, w)
			delete(r.pending, caller)
		}
	}
	r.mu.Unlock()

	for _, w := range ready {
		ev := w.ev
		// ts is the moment the window CLOSED, suppressed_since is the
		// first refusal in it. The row is final when written and is never
		// updated, which is what lets both fields sit in the hash scope.
		ev.TS = now
		ev.SuppressedCount = w.count
		ev.SuppressedSince = w.since
		if w.mixed {
			ev.TargetWorkflow = ""
			ev.TargetRef = ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		r.Write(ctx, ev)
		cancel()
	}
}

// Pending is the number of open coalescing windows, for the admin health
// surface: a value that never drops to zero means the flush loop has stalled.
func (r *rejectLogger) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}
