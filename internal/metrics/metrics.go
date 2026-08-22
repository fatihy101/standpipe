// Package metrics is the gateway's Prometheus registry.
//
// It is deliberately hand-written rather than client_golang: the exposition
// this project needs is one text endpoint on the admin listener, and the
// dependency would be the largest in the tree for that.
//
// # Cardinality is the design constraint
//
// `repo`, `workflow` and `caller` labels are emitted ONLY after
// authentication and allowlist validation, so a stranger's POST cannot inject
// label values, and there is NO `source_ip` label anywhere. This package cannot
// enforce the first rule — the call site does — but it does enforce a hard
// per-metric series cap so a mistake degrades into a recorded drift entry
// instead of an unbounded map.
//
// # Drift
//
// The metrics table used to be hand-kept, and it once lost five counters and
// three labels with nothing failing. The catalog below is that table. Anything recorded under a name the catalog
// does not know, or with a label set the catalog does not declare, is still
// recorded — dropping it would be a silent degradation, the first failure mode
// this project guards against — but it is also remembered in Drift(), which the admin /healthz
// surfaces. The table drifting is then visible without anyone diffing it.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Kind is a metric's Prometheus type.
type Kind string

const (
	KindCounter   Kind = "counter"
	KindGauge     Kind = "gauge"
	KindHistogram Kind = "histogram"
)

// Def is one row of the metrics catalog.
type Def struct {
	Name    string
	Kind    Kind
	Help    string
	Labels  []string
	Buckets []float64

	// NotEmitted carries the catalog's "†" — declared in the table, emitted by no
	// code path yet. The state has to be carried rather than dropped: a row
	// nobody implements is invisible to Drift(), which only ever sees what the
	// code recorded, so dropping it is precisely how such a row goes missing.
	//
	// It is load-bearing in two places. New() does not seed a zero series for
	// it, so a metric the table says is not emitted is also absent from
	// /metrics — otherwise the transcription would put a line on the wire and
	// make the table's own claim false. And recording under such a name is
	// itself a drift entry: the code overtook the table, which is the same
	// class of disagreement as an undeclared name.
	NotEmitted bool
}

// DefaultBuckets covers a gateway hop plus a 10s upstream timeout.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

// writeBuckets are for SQLite writes, which are sub-millisecond when healthy
// and seconds when busy_timeout is being burned.
var writeBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5}

// Catalog is the metrics table, in its order — all thirty rows, name, type and
// label set.
//
// It began as a hand transcription of a document, and on 2026-08-22 three of
// its rows were wrong: gateway_target_unavailable carried a {workflow} label the
// table had already removed, and gateway_pre_auth_lru_evicted_total and
// gateway_run_name_absent_total were missing outright. The middle one is
// the instructive failure — the label set the table declares and the label set
// internal/forge/identity.go emits AGREED, and the transcription alone made
// Drift() non-empty, on the very field the admin /healthz publishes as metrics_drift. A
// runtime drift surface cannot audit the copy that defines it, so the copy has
// to be compared against the table itself by something executable.
//
// The two earlier code-side divergences recorded here are both closed and the
// note is kept so neither is reopened by memory: the table declares
// gateway_github_secondary_rate_limit_total{installation_id} and client.go now
// emits that, and gateway_target_unavailable is {repo} on both sides (a gauge
// may not carry a label its reset path cannot bind).
//
// One row disagreed with the original design document, and it was the
// document's side: it marked gateway_watched_ip_rejected_total with "†",
// declared and not yet emitted, while internal/httpapi/trigger.go already
// emitted it. Only the annotation was stale — the row's name, type and labels
// are correct — so the row is carried without NotEmitted. Setting the flag would put a permanent entry in Drift() for every
// deployment that configures observability.watch_ips, which is the noise this
// transcription was repaired to remove.
func Catalog() []Def {
	return []Def{
		{Name: "gateway_trigger_total", Kind: KindCounter, Labels: []string{"repo", "workflow", "status"},
			Help: "Trigger requests that reached target validation, by trigger_outcome outcome_status. NOT an HTTP code."},
		{Name: "gateway_trigger_duration_seconds", Kind: KindHistogram, Labels: []string{"repo", "workflow"},
			Help: "End-to-end gateway handling time for a trigger request.", Buckets: DefaultBuckets},
		{Name: "gateway_idempotency_replay_total", Kind: KindCounter,
			Help: "Requests answered from a cached idempotent response."},
		{Name: "gateway_idempotency_conflict_total", Kind: KindCounter,
			Help: "409 idempotency_conflict: same key, different body or path."},
		{Name: "gateway_upstream_errors_total", Kind: KindCounter, Labels: []string{"upstream_status"},
			Help: "Dispatch calls that failed upstream, by the GitHub status (0 when there was no response)."},
		{Name: "gateway_caller_requests_total", Kind: KindCounter, Labels: []string{"caller"},
			Help: "Authenticated trigger requests per caller."},
		{Name: "gateway_github_rate_remaining", Kind: KindGauge, Labels: []string{"bucket"},
			Help: "GitHub quota remaining per quota bucket. 'app' is the App JWT bucket."},
		{Name: "gateway_github_rate_reset_timestamp", Kind: KindGauge, Labels: []string{"bucket"},
			Help: "Unix timestamp at which the quota bucket resets."},
		{Name: "gateway_sqlite_write_duration_seconds", Kind: KindHistogram, Labels: []string{"table"},
			Help: "Duration of a durable write, by the table it targets.", Buckets: writeBuckets},
		{Name: "gateway_uptime_seconds", Kind: KindGauge, Help: "Seconds since the gateway process started."},
		{Name: "gateway_inventory_coverage_gap_pct", Kind: KindGauge,
			Help: "Coverage gap percentage. Never reaches zero; the structural half is by design."},
		{Name: "gateway_inventory_coverage_unknown_pct", Kind: KindGauge,
			Help: "Unknown-coverage percentage. Never summed with the gap gauge."},
		// {repo} only. The gauge-label rule: a gauge's label set must be exactly the
		// dimensions BOTH the path that writes 1 and the path that writes 0
		// hold. Installation reachability is a repo-level fact and recovery
		// writes a single 0; a {workflow} label would clear one series and
		// leave every other workflow of that repo pinned at 1 for the process
		// lifetime. An alarm that never goes out is worse than a missing
		// dimension.
		{Name: "gateway_target_unavailable", Kind: KindGauge, Labels: []string{"repo"},
			Help: "1 when the App is not installed on the target or it cannot be reached."},
		{Name: "gateway_auth_failures_total", Kind: KindCounter,
			Help: "Requests refused before authentication succeeded. Carries no IP label."},

		// The single exception to the cardinality rule. Emitted only
		// for an address written in observability.watch_ips, and the label
		// value is the CONFIGURED entry rather than the observed IP, so the
		// ceiling is 32 × |reason| = 64 series and belongs to configuration,
		// not to callers. With the default empty list the metric never appears.
		{Name: "gateway_watched_ip_rejected_total", Kind: KindCounter, Labels: []string{"ip", "reason"},
			Help: "Pre-auth rejections from an IP on observability.watch_ips."},

		// †: declared, emitted by nothing. The pre-auth IP limiter
		// is a fixed-capacity LRU (10,000 entries) and an evicted entry resets
		// that IP's count, which is how a rotating source outlives the limit —
		// a degradation with no error and no external symptom. Unlabelled on purpose: which IP was evicted is exactly what
		// must not be published, and the question is whether the store is under
		// pressure. Carried here so the row is not lost again; NotEmitted keeps
		// it off the wire until it is implemented.
		{Name: "gateway_pre_auth_lru_evicted_total", Kind: KindCounter, NotEmitted: true,
			Help: "Pre-auth IP limiter LRU evictions. Declared in the catalog, not emitted yet; non-zero reopens the fixed-capacity decision."},

		{Name: "gateway_workflow_cache_cold_total", Kind: KindCounter, Labels: []string{"repo", "workflow"},
			Help: "Requests served against a never-fetched workflow definition, so input validation was skipped."},
		{Name: "gateway_github_secondary_rate_limit_total", Kind: KindCounter, Labels: []string{"installation_id"},
			Help: "GitHub secondary rate limits observed. Arrives as 403, not 429."},
		{Name: "gateway_bootstrap_truncated_total", Kind: KindCounter, Labels: []string{"repo"},
			Help: "Bootstrap history slices cut short by the page cap."},
		{Name: "gateway_clock_jump_suspected_total", Kind: KindCounter,
			Help: "Idempotency purges skipped because a forward clock jump was suspected."},
		{Name: "gateway_trigger_rejected_total", Kind: KindCounter, Labels: []string{"caller", "reason"},
			Help: "Authenticated requests refused before reservation. Counted PER REQUEST; the event is coalesced per window for 429s."},
		{Name: "gateway_reject_log_dropped_total", Kind: KindCounter,
			Help: "trigger_rejected events that could not be written. The request was still rejected."},
		{Name: "gateway_correlation_total", Kind: KindCounter, Labels: []string{"correlation"},
			Help: "Dispatches by correlation armament: what we armed, not what resolved."},
		{Name: "gateway_run_link_total", Kind: KindCounter, Labels: []string{"resolution", "resolution_source"},
			Help: "run_link events by reliability and by the tier that produced them."},

		// Emitted from internal/forge/resolver.go, and its absence from this
		// catalog once made Drift() blame the code for a name the table blesses.
		// Read it as a rate and never as an alarm: the reference lists four states it
		// cannot tell apart, and a healthy tier-1 deployment leaves it at zero
		// because tier 2 is never reached.
		{Name: "gateway_run_name_absent_total", Kind: KindCounter, Labels: []string{"repo", "workflow"},
			Help: "Correlation tier 2 reached its horizon with a candidate run on the ref, and no candidate carried the injected id."},

		{Name: "gateway_correlation_sweep_error_total", Kind: KindCounter,
			Help: "Resolver sweeps that ended in error (correlation tier 3). Unlabelled by design."},
		{Name: "gateway_github_dispatch_total", Kind: KindCounter, Labels: []string{"repo", "workflow", "outcome"},
			Help: "Outcome of the upstream dispatch CALL. Not the same population as gateway_trigger_total."},
		{Name: "gateway_github_quota_paused_total", Kind: KindCounter, Labels: []string{"bucket"},
			Help: "GitHub quota pauses. The only signal that inventory refresh stopped rather than found nothing."},
		{Name: "gateway_github_queue_timeout_total", Kind: KindCounter, Labels: []string{"lane"},
			Help: "github.max_queue_wait exceeded, split by lane so a slow caller and a saturated background are distinguishable."},
		{Name: "gateway_github_token_refresh_failed_total", Kind: KindCounter, Labels: []string{"repo"},
			Help: "Installation token renewal failed while the old token was still valid — a degradation that looks like success."},
	}
}

// maxSeriesPerMetric bounds one metric's label-value combinations. It is a
// backstop for a cardinality mistake at a call site, not the primary defence:
// the primary defence is the rule that repo/workflow/caller are emitted
// only after authn and allowlist validation.
const maxSeriesPerMetric = 4096

type series struct {
	labels  []string // flattened k,v pairs, sorted by key
	value   float64
	counts  []uint64 // histogram bucket counts, cumulative on write
	sum     float64
	total   uint64
	present bool
}

type metric struct {
	def    Def
	series map[string]*series
	capped bool
}

// Registry holds every metric. It is safe for concurrent use and satisfies the
// consumer-side Metrics interface internal/forge declares.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*metric
	order   []string
	drift   map[string]string

	start     time.Time
	collect   []func()
	watchIPs  map[string]struct{}
	nowSource func() time.Time
}

// New builds a registry seeded from Catalog.
func New() *Registry {
	r := &Registry{
		metrics:   make(map[string]*metric),
		drift:     make(map[string]string),
		start:     time.Now(),
		nowSource: time.Now,
	}
	for _, d := range Catalog() {
		m := &metric{def: d, series: make(map[string]*series)}
		r.metrics[d.Name] = m
		r.order = append(r.order, d.Name)
		if len(d.Labels) == 0 && d.Kind == KindCounter && !d.NotEmitted {
			// A counter that has never fired must still be exported as 0.
			// Absence and zero are different facts and a dashboard cannot
			// tell them apart from a missing series.
			//
			// NotEmitted is the one exception, and it is not an oversight: a
			// row the catalog marks "declared, not emitted yet" must not be put on
			// the wire by the act of transcribing it. Write() skips a metric
			// with no series, so the row stays in the catalog and off /metrics
			// until a call site starts recording it.
			m.series[""] = &series{present: true}
		}
	}
	return r
}

// SetWatchIPs installs observability.watch_ips. Only these addresses ever
// reach a label.
//
// The strings are taken as given and compared with exact equality — the rule
// says "verbatim". That is only correct because internal/config
// canonicalises every entry at load, into the same form internal/httpapi's
// clientIP produces for the observed address. There is deliberately no second
// normaliser here: one address spelled two ways would then match in this
// package and not in the event record, which is a worse bug than the one that
// made this comment necessary.
//
// Consequence for callers: a Registry fed a raw config entry rather than a
// config.Load'ed one silently watches nothing.
func (r *Registry) SetWatchIPs(ips []string) {
	set := make(map[string]struct{}, len(ips))
	for _, ip := range ips {
		if ip != "" {
			set[ip] = struct{}{}
		}
	}
	r.mu.Lock()
	r.watchIPs = set
	r.mu.Unlock()
}

// Watched reports whether ip is on the watch list.
func (r *Registry) Watched(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.watchIPs[ip]
	return ok
}

// IncWatchedIP records a pre-auth rejection for a watched IP and does nothing
// for any other address.
func (r *Registry) IncWatchedIP(ip, reason string) {
	if !r.Watched(ip) {
		return
	}
	r.IncCounter("gateway_watched_ip_rejected_total", map[string]string{"ip": ip, "reason": reason})
}

// OnCollect registers a function run immediately before an exposition is
// written. Gauges whose truth lives elsewhere — quota snapshots, uptime — are
// refreshed there rather than polled on a timer.
func (r *Registry) OnCollect(fn func()) {
	r.mu.Lock()
	r.collect = append(r.collect, fn)
	r.mu.Unlock()
}

// IncCounter adds one. Part of internal/forge's Metrics interface.
func (r *Registry) IncCounter(name string, labels map[string]string) {
	r.AddCounter(name, 1, labels)
}

// AddCounter adds a non-negative delta.
func (r *Registry) AddCounter(name string, delta float64, labels map[string]string) {
	if delta < 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.locked(name, KindCounter, labels)
	if s == nil {
		return
	}
	s.value += delta
}

// SetGauge sets a gauge. Part of internal/forge's Metrics interface.
func (r *Registry) SetGauge(name string, value float64, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.locked(name, KindGauge, labels)
	if s == nil {
		return
	}
	s.value = value
}

// Observe records a histogram sample, in seconds.
func (r *Registry) Observe(name string, value float64, labels map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, s := r.lockedMetric(name, KindHistogram, labels)
	if s == nil {
		return
	}
	if s.counts == nil {
		s.counts = make([]uint64, len(m.def.Buckets))
	}
	for i, b := range m.def.Buckets {
		if value <= b {
			s.counts[i]++
		}
	}
	s.sum += value
	s.total++
}

// ObserveDuration is Observe with a time.Duration.
func (r *Registry) ObserveDuration(name string, d time.Duration, labels map[string]string) {
	r.Observe(name, d.Seconds(), labels)
}

func (r *Registry) locked(name string, kind Kind, labels map[string]string) *series {
	_, s := r.lockedMetric(name, kind, labels)
	return s
}

func (r *Registry) lockedMetric(name string, kind Kind, labels map[string]string) (*metric, *series) {
	m, ok := r.metrics[name]
	if !ok {
		// Undeclared: still recorded, and remembered. Dropping it would hide
		// a real event; refusing to remember it would hide the table drift.
		r.driftf(name, "not in the metrics catalog")
		def := Def{Name: name, Kind: kind, Help: "undeclared metric; not in the metrics catalog"}
		if kind == KindHistogram {
			def.Buckets = DefaultBuckets
		}
		for k := range labels {
			def.Labels = append(def.Labels, k)
		}
		sort.Strings(def.Labels)
		m = &metric{def: def, series: make(map[string]*series)}
		r.metrics[name] = m
		r.order = append(r.order, name)
	}
	if m.def.Kind != kind {
		r.driftf(name, fmt.Sprintf("recorded as %s, declared %s", kind, m.def.Kind))
	}
	if m.def.NotEmitted {
		// The catalog marks this row "declared, not emitted yet" and a call site
		// just recorded it. The code overtook the table; that is the same
		// class of disagreement as an undeclared name and belongs in the same
		// place. gateway_watched_ip_rejected_total went stale exactly this way
		// and nothing said so.
		r.driftf(name, "declared not-emitted (†) in the catalog, but recorded")
	}
	r.checkLabels(m, labels)

	key, flat := labelKey(labels)
	s, ok := m.series[key]
	if ok {
		return m, s
	}
	if len(m.series) >= maxSeriesPerMetric {
		if !m.capped {
			m.capped = true
			r.driftf(name, fmt.Sprintf("series cap %d reached; new label combinations are dropped", maxSeriesPerMetric))
		}
		return m, nil
	}
	s = &series{labels: flat, present: true}
	m.series[key] = s
	return m, s
}

func (r *Registry) checkLabels(m *metric, labels map[string]string) {
	if len(labels) != len(m.def.Labels) {
		r.driftf(m.def.Name, fmt.Sprintf("label set %v, declared %v", sortedKeys(labels), m.def.Labels))
		return
	}
	for _, want := range m.def.Labels {
		if _, ok := labels[want]; !ok {
			r.driftf(m.def.Name, fmt.Sprintf("label set %v, declared %v", sortedKeys(labels), m.def.Labels))
			return
		}
	}
}

// driftf records a disagreement once per metric+message. Callers hold r.mu.
func (r *Registry) driftf(name, msg string) {
	key := name + ": " + msg
	if _, ok := r.drift[key]; ok {
		return
	}
	r.drift[key] = msg
}

// Drift lists every disagreement between what the catalog declares and what
// the code recorded, plus every cardinality cap that was hit. Empty is the
// healthy state; the admin /healthz publishes it when it is not.
func (r *Registry) Drift() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.drift))
	for k := range r.drift {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Uptime is gateway_uptime_seconds' source.
func (r *Registry) Uptime() time.Duration { return r.nowSource().Sub(r.start) }

func sortedKeys(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for k := range labels {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func labelKey(labels map[string]string) (string, []string) {
	if len(labels) == 0 {
		return "", nil
	}
	keys := sortedKeys(labels)
	flat := make([]string, 0, len(keys)*2)
	var b strings.Builder
	for _, k := range keys {
		flat = append(flat, k, labels[k])
		b.WriteString(k)
		b.WriteByte(0x1f)
		b.WriteString(labels[k])
		b.WriteByte(0x1e)
	}
	return b.String(), flat
}

// Write writes the Prometheus text exposition.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	collect := append([]func(){}, r.collect...)
	r.mu.Unlock()
	for _, fn := range collect {
		fn()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	for _, name := range r.order {
		m := r.metrics[name]
		if len(m.series) == 0 {
			continue
		}
		if m.def.Help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(m.def.Help))
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, m.def.Kind)

		keys := make([]string, 0, len(m.series))
		for k := range m.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			s := m.series[k]
			switch m.def.Kind {
			case KindHistogram:
				var cumulative uint64
				for i, bound := range m.def.Buckets {
					if i < len(s.counts) {
						cumulative = s.counts[i]
					}
					fmt.Fprintf(&b, "%s_bucket%s %d\n", name, renderLabels(s.labels, "le", formatFloat(bound)), cumulative)
				}
				fmt.Fprintf(&b, "%s_bucket%s %d\n", name, renderLabels(s.labels, "le", "+Inf"), s.total)
				fmt.Fprintf(&b, "%s_sum%s %s\n", name, renderLabels(s.labels, "", ""), formatFloat(s.sum))
				fmt.Fprintf(&b, "%s_count%s %d\n", name, renderLabels(s.labels, "", ""), s.total)
			default:
				fmt.Fprintf(&b, "%s%s %s\n", name, renderLabels(s.labels, "", ""), formatFloat(s.value))
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler serves /metrics on the admin listener.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.Write(w)
	})
}

func renderLabels(flat []string, extraKey, extraVal string) string {
	if len(flat) == 0 && extraKey == "" {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(flat); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(flat[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(flat[i+1]))
		b.WriteString(`"`)
	}
	if extraKey != "" {
		if len(flat) > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extraKey)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(extraVal))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeLabel(s string) string { return labelEscaper.Replace(s) }
func escapeHelp(s string) string  { return helpEscaper.Replace(s) }

func formatFloat(f float64) string {
	if f == float64(int64(f)) && f < 1e15 && f > -1e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
