// Package forge is the gateway's only point of contact with GitHub.
//
// Nothing outside this package speaks to the GitHub API.
//
// Layout:
//
//	config.go     configuration subset this package consumes
//	errors.go     typed upstream errors and their mapping to gateway
//	              responses (including the upstream-timeout rule)
//	identity.go   GitHub App JWT, installation lookup, installation tokens
//	limiter.go    bounded-concurrency, per-installation quota accounting
//	client.go     the client every call goes through
//	dispatch.go   dispatch request mapping, tier-1/tier-2 correlation
//	              arming rules
//	ref.go        SEAM: the dispatch ref form — VERIFIED 2026-08-21
//	runname.go    SEAM: the run-name field accessor — VERIFIED 2026-08-21
//	              (correlation tier 2)
//	dispatchlatency.go
//	              SEAM: whether return_run_details holds the request open —
//	              VERIFIED 2026-08-21 (correlation tier 1)
//	inventory.go  surface and history discovery, the github_runs
//	              bootstrap cache
//	resolver.go   the query-based run_id correlation resolver
//	resolveone.go the pre-retry guard: one trigger, the ORIGINAL window
//	store.go      consumer-side storage interfaces (internal/store implements)
//	metrics.go    consumer-side metrics interface
//
// Three behaviours in here were once assumptions, each isolated behind a single
// switch so that correcting it would be a one-line change: see ref.go,
// runname.go and dispatchlatency.go. ALL THREE WERE MEASURED on 2026-08-21 by
// four real dispatches against a private throwaway repo, all three
// "unverified" markers are CLOSED, and NONE of the three constants had to
// move.
//
// The switches remain, and they are now verified defaults with an escape hatch
// rather than open questions: GitHub can change under us and a one-line flip is
// still the right shape of response. Each file records what was measured, when,
// and how. What the probe did NOT cover is written there too
// as named limits — one repo, four calls, an idle account, one moment — and in
// particular `run-name` application is known to vary by EVENT TYPE, which is
// why tier 2's silent-death detector (resolver.go's tier2Absent and
// gateway_run_name_absent_total) was NOT removed as answered.
//
// The run_id correlation hierarchy is split across three files and it is worth
// knowing where each tier lives before changing any of them:
//
//	tier 1  dispatch.go  — return_run_details on the request, run id off the
//	                       response; the run_link is written by the composition
//	                       root on the request path (RunLinkFromDispatch)
//	tier 2  resolver.go  — the standby_trigger_id run-name search, gated on
//	                       Correlation.ArmsRunNameMatch
//	tier 3  resolver.go  — the time window
//
// Tiers 2 and 3 have a second entry point in resolveone.go. The batch sweep is
// driven by a query over FORWARDED triggers, so it structurally cannot see a
// trigger whose outcome is `error` — which is every reclaimed `unknown`
// retry. ResolveOne serves that one case, read-only, against the superseded
// attempt's window, and its blind spots are written out on the function.
//
// Correlation (dispatch.go) records which tier was ARMED, before the call.
// ResolutionSource (store.go) records which tier FIRED, after it. They are
// deliberately different facts; see correlationFor for why the first one is
// ordered by durability rather than by strength.
package forge
