package forge

import "time"

// ---------------------------------------------------------------------------
// SEAM 3 of 3 — the latency profile of return_run_details. VERIFIED DEFAULT,
// escape hatch kept.
//
// ANSWERED BY MEASUREMENT on 2026-08-21. The tier-1 "unverified" marker
// is CLOSED. Do not reopen it as an investigation;
// moving this constant needs a NEW measurement.
//
// WHAT WAS MEASURED. Four real dispatches against a private throwaway repo, wall clock:
//
//	return_run_details: true                 200 + run details   1.375 s
//	return_run_details: true (tag, full ref)  200                1.427 s
//	return_run_details: true (tag, short ref) 200                1.352 s
//	parameter omitted                         204, zero bytes    1.652 s
//
// THE PARAMETER ADDS NO LATENCY. ~1.4 s is the BASE cost of a dispatch; the
// call without the parameter was, on this sample, the slower one. The feared
// "blocking model multiplies upstream_timeout" behaviour does not occur.
// LatencyIndependent is confirmed, the constant did not have to move, and the
// code was not touched by that slice.
//
// THE SECOND FINDING, WHICH WAS NOT WHAT THE MARKER ASKED AND MATTERS MORE.
// return_run_details ELIMINATES AN ASYNC GAP THAT IS OTHERWISE REAL. With the
// parameter, the workflow_run row EXISTS when the call returns: the returned
// run id was read immediately and the row was there. WITHOUT it, a listing made
// immediately after the 204 found NOTHING, and one about two seconds later
// found the run. So tier 1 does not only save a GitHub query — it closes the
// window between "dispatch accepted" and "run listable". Resolver-side
// consequence: ResolveOne's blind spot 1 (resolveone.go).
//
// WHAT IS STILL NOT KNOWN — NAMED LIMITS, NOT MARKERS:
//
//   - The ~1.4 s baseline was taken on an IDLE account with an EMPTY Actions
//     queue. It says "the parameter is not a multiplier", NOT "a dispatch
//     always takes 1.4 s". A loaded installation may have a different base
//     cost, so this is not licence to TIGHTEN gateway.upstream_timeout from
//     10s. Config.UpstreamTimeout's own comment holds that line too.
//   - The async gap was BOUNDED by two polls, not characterised: greater than
//     zero, under roughly two seconds, on one repo. NOBODY SHOULD TUNE A
//     TICKER TO TWO SECONDS off this number.
//
// TO CORRECT: change DispatchLatencyModel below (one line). Nothing else in the
// package decides the dispatch deadline; every dispatch goes through
// dispatchTimeout, and httpClientTimeout keeps the shared http.Client from
// capping it. The seam stays because GitHub can change under us and the
// blocking branch is a one-line response, not because the answer is in doubt.
// ---------------------------------------------------------------------------

// LatencyModel names what the dispatch endpoint does with the request while
// the run record is being created. Which one holds is measured, not assumed —
// see the block comment above.
type LatencyModel int

const (
	// LatencyIndependent: the response comes back as promptly as the old 204
	// did; asking for run details does not add server-side waiting. MEASURED
	// TRUE on 2026-08-21 — 1.375/1.427/1.352 s with the parameter against
	// 1.652 s without it — and the model the 10s upstream timeout is sized for.
	LatencyIndependent LatencyModel = iota
	// LatencyBlocking: GitHub holds the request until the run row exists, so
	// dispatch latency tracks Actions' queue depth. Measured NOT to be the
	// case today; retained because it is what a future regression would look
	// like. Selecting it gives the dispatch its own, larger budget — see
	// dispatchTimeout.
	LatencyBlocking
)

// DispatchLatencyModel is the single switch. See the block comment above: the
// value is measured-correct as of 2026-08-21, not assumed.
const DispatchLatencyModel = LatencyIndependent

// blockingDispatchTimeoutFactor is how much more room the dispatch gets under
// LatencyBlocking, as a multiple of gateway.upstream_timeout.
//
// It is a multiple rather than a new config key on purpose: the configuration
// schema is not this package's to extend, and inventing
// github.dispatch_timeout here would be a design change made in code. The 2026-08-21 measurement says
// this branch is not needed today. If a future measurement shows the blocking
// model has appeared, the right fix is still to report the number and let the
// configuration design decide — this constant is only the shape of the fix.
const blockingDispatchTimeoutFactor = 3

// dispatchTimeout is the deadline budget for POST .../dispatches. Every
// dispatch goes through it; nothing else in the package does.
func dispatchTimeout(cfg Config) time.Duration {
	if DispatchLatencyModel == LatencyBlocking && cfg.tier1Armed() {
		return cfg.UpstreamTimeout * blockingDispatchTimeoutFactor
	}
	return cfg.UpstreamTimeout
}

// httpClientTimeout is the ceiling on the shared per-repo http.Client.
//
// It is the widest budget any single call may take, NOT the per-call budget:
// http.Client.Timeout is a hard cap that would silently truncate an extended
// dispatch deadline, which would make flipping DispatchLatencyModel look like
// it worked while changing nothing. Per-call deadlines are set by call and
// callWithTimeout.
func httpClientTimeout(cfg Config) time.Duration {
	if d := dispatchTimeout(cfg); d > cfg.UpstreamTimeout {
		return d
	}
	return cfg.UpstreamTimeout
}
