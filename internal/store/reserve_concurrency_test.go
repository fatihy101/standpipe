package store

import (
	"fmt"
	"sync"
	"testing"
)

// TestReserveConcurrentSingleKey is the test the product exists for.
//
// N goroutines race for the same (caller_name, idempotency_key). Exactly one
// may come back with an outcome that permits a dispatch; every other one must
// be told in_progress. Two `new` outcomes here is a double dispatch in
// production.
//
// Run it under -race.
func TestReserveConcurrentSingleKey(t *testing.T) {
	const goroutines = 32

	env := newTestEnv(t)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	c := ctx(t)

	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		results = make([]ReserveResult, 0, goroutines)
		errs    []error
	)
	start.Add(1)
	done.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer done.Done()
			start.Wait()
			req := reserveReq("acme", "key-race", "POST /t/acme/core/release.yml", body,
				fmt.Sprintf("evt_race_%02d", i))
			res, err := env.Reserve(c, req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, res)
		}(i)
	}
	start.Done()
	done.Wait()

	if len(errs) != 0 {
		t.Fatalf("Reserve returned %d errors, want 0: %v", len(errs), errs)
	}

	var newCount, inProgress int
	var winner ReserveResult
	for _, r := range results {
		switch r.Outcome {
		case ReserveNew:
			newCount++
			winner = r
		case ReserveInProgress:
			inProgress++
			if r.Outcome.MayDispatch() {
				t.Errorf("ReserveInProgress reported MayDispatch() == true")
			}
		default:
			t.Errorf("unexpected outcome %q on a virgin key", r.Outcome)
		}
	}
	if newCount != 1 {
		t.Fatalf("got %d ReserveNew outcomes, want exactly 1 — this is a double dispatch", newCount)
	}
	if inProgress != goroutines-1 {
		t.Fatalf("got %d in_progress outcomes, want %d", inProgress, goroutines-1)
	}

	// The row must belong to the single winner, and exactly one trigger event
	// may exist: the protocol writes no event for an in_progress answer.
	rec, err := env.LookupIdempotency(c, "acme", "key-race")
	if err != nil {
		t.Fatalf("LookupIdempotency: %v", err)
	}
	if rec.State != StateInFlight {
		t.Errorf("row state = %q, want in_flight", rec.State)
	}
	if rec.EventID != winner.EventID {
		t.Errorf("row event_id = %q, want the winner's %q", rec.EventID, winner.EventID)
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM trigger_events WHERE event_type = 'trigger'`); n != 1 {
		t.Errorf("got %d trigger events, want 1 — a losing attempt logged a dispatch that never happened", n)
	}
}

// TestReserveConcurrentDistinctKeys is the control: N distinct keys must all
// win. A serialised writer that accidentally answered in_progress for
// unrelated keys would pass the test above and fail this one.
func TestReserveConcurrentDistinctKeys(t *testing.T) {
	const goroutines = 16

	env := newTestEnv(t)
	body := bodyHash(`{"inputs":{}}`)
	c := ctx(t)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out = map[ReserveOutcome]int{}
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			req := reserveReq("acme", fmt.Sprintf("key-%02d", i),
				"POST /t/acme/core/release.yml", body, fmt.Sprintf("evt_dk_%02d", i))
			res, err := env.Reserve(c, req)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			out[res.Outcome]++
		}(i)
	}
	wg.Wait()

	if out[ReserveNew] != goroutines {
		t.Fatalf("outcomes = %v, want %d x new", out, goroutines)
	}
}

// TestReclaimConcurrent races N retries against a single `unknown` row. The
// reservation CAS must hand the row to exactly one of them.
func TestReclaimConcurrent(t *testing.T) {
	const goroutines = 16

	env := newTestEnv(t)
	c := ctx(t)
	body := bodyHash(`{"inputs":{"tag":"v1"}}`)
	path := "POST /t/acme/core/release.yml"

	first, err := env.Reserve(c, reserveReq("acme", "key-cas", path, body, "evt_cas_first"))
	if err != nil || first.Outcome != ReserveNew {
		t.Fatalf("seed Reserve = %v, %v; want new", first.Outcome, err)
	}
	if _, err := env.Complete(c, completeReq("acme", "key-cas", "evt_cas_first", StateUnknown, 0, "")); err != nil {
		t.Fatalf("seed Complete: %v", err)
	}

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		out = map[ReserveOutcome]int{}
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			res, err := env.Reserve(c, reserveReq("acme", "key-cas", path, body,
				fmt.Sprintf("evt_cas_%02d", i)))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			out[res.Outcome]++
		}(i)
	}
	wg.Wait()

	if out[ReserveReclaimed] != 1 {
		t.Fatalf("outcomes = %v, want exactly 1 reclaimed — more than one retry dispatched", out)
	}
	if out[ReserveInProgress] != goroutines-1 {
		t.Fatalf("outcomes = %v, want %d in_progress", out, goroutines-1)
	}
}
