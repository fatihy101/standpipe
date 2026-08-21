package forge

// Metrics is the consumer-side view of the metrics registry. internal/metrics
// owns the real implementation; this package neither imports it nor assumes
// its signatures.
//
// This list is the whole of what package forge emits, and every entry is
// checked against the metrics catalog, internal/metrics.Catalog(). A name
// or a label set that appears in one and not the other surfaces at runtime through that package's Drift() — the list
// below is not the authority, it is the map.
//
// Counters this package emits:
//
//	gateway_github_quota_paused_total{bucket}          (limiter.go)
//	gateway_github_queue_timeout_total{lane}           (limiter.go)
//	gateway_github_secondary_rate_limit_total{installation_id}
//	                                                   (client.go; the
//	                                                    secondary limit is
//	                                                    applied per
//	                                                    installation, so the
//	                                                    label is the bucket it
//	                                                    throttles)
//	gateway_github_token_refresh_failed_total{repo}    (identity.go)
//	gateway_github_dispatch_total{repo,workflow,outcome}
//	                                                   (dispatch.go)
//	gateway_workflow_cache_cold_total{repo,workflow}   (dispatch.go and
//	                                                    inventory.go)
//	gateway_bootstrap_truncated_total{repo}            (inventory.go)
//	gateway_correlation_total{correlation}             (armed tier, on
//	                                                    the dispatch path)
//	gateway_run_link_total{resolution,resolution_source}
//	                                                   (correlation result,
//	                                                    from the resolver; the
//	                                                    tier-1 link is written
//	                                                    and counted by the
//	                                                    caller)
//	gateway_correlation_sweep_error_total              (resolver.go)
//	gateway_run_name_absent_total{repo,workflow}       (resolver.go; see the
//	                                                    note below)
//
// Gauges:
//
//	gateway_github_rate_remaining{bucket}              (limiter.go)
//	gateway_target_unavailable{repo}                   (identity.go)
//
// gateway_run_name_absent_total was added to the catalog after this package
// started emitting it, because the failure it counts has no other symptom:
// tier 2 failing to find the injected id drops the trigger to tier 3, where it
// is recorded `inferred` or `ambiguous` with no error, a silent degradation.
// Measurement on 2026-08-21 showed GitHub does not always apply a declared
// `run-name:` (runname.go), so the case is real. The counter fires once per
// trigger, at the horizon, and only when the window held a run the trigger
// could own; see tier2Outcome in resolver.go for exactly what it does and does
// not prove.
//
// Its cardinality is bounded like the other repo/workflow counters: the
// resolver only ever sees triggers that were authenticated and allowlisted
// before they were stored, so no label value is attacker-chosen.
//
// gateway_target_unavailable carries {repo} only. Installation availability is
// a repo-level fact with no workflow in scope at the emission site, and a
// workflow label would leave the gauge stuck at 1 forever for every workflow
// but the one that recovered. The reasoning is written out at the emission
// site in identity.go.
//
// ResolveOne (resolveone.go) emits NOTHING. It is a read-only guard whose
// result the caller acts on, and the caller — which owns the completion write
// and the catalog — is where that fact belongs as a counter.
type Metrics interface {
	IncCounter(name string, labels map[string]string)
	SetGauge(name string, value float64, labels map[string]string)
}

// nopMetrics is used when no registry is supplied. It exists so no call site
// needs a nil check — a missing registry must not change behaviour.
type nopMetrics struct{}

func (nopMetrics) IncCounter(string, map[string]string)        {}
func (nopMetrics) SetGauge(string, float64, map[string]string) {}
