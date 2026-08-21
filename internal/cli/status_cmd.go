package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// statusWindow is the reporting window of the TRIGGERS and CORRELATION
// blocks. `status` prints "TRIGGERS (last 24h)"; the same window is used for the
// correlation mix so the two lines describe the same population.
const statusWindow = 24 * time.Hour

// The three thresholds behind `status`'s exit code.
//
// They are deliberately NOT configurable in Phase 0, because a configurable
// threshold is a policy surface and policy is Phase 1's `policy` block. Making
// them configurable here would be the first piece of that surface arriving
// without the design discussion it requires.
//
// None of the three is invented. Each is an EXISTING constant with an existing
// meaning, so `status` turns red at exactly the moment the mechanism it
// describes changes behaviour:
//
//   - quota: forge.DefaultQuotaPauseThreshold is where the rate-limit guard PAUSES background
//     inventory work. Below it the coverage number silently stops advancing,
//     which is the degradation `status` exists to expose.
//   - unresolved: forge.DefaultResolverHorizon is where the correlation resolver gives up and
//     writes resolution `none`. A trigger older than that with no run_link at
//     all means the resolver is not running, not that it is still working.
//   - cold cache: any cold workflow is a threshold of one, because a cold
//     cache means input validation is SKIPPED for that target. There is
//     no healthy amount of that.
const (
	statusQuotaLow         = forge.DefaultQuotaPauseThreshold
	statusResolverHorizon  = forge.DefaultResolverHorizon
	statusWorkflowCacheTTL = forge.DefaultWorkflowCacheTTL
)

// runStatusCmd implements `standpipe status`.
//
// The line against /healthz: /healthz asks whether the process is
// up, `status` asks whether the system is doing its job. That is why this
// command combines the running gateway's gauges with the durable state on
// disk: neither source alone can answer it.
//
// It defines no new measurement. Every line has a source
// already specified: INSTALLATIONS from gateway_github_rate_remaining,
// WORKFLOW CACHE from workflow_cache.fetched_at, TRIGGERS from the event log,
// CORRELATION from trigger_events.correlation, COVERAGE from the coverage query.
func runStatusCmd(args []string) error {
	fs := newFlagSet("status")
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	adminURL := fs.String("admin-url", "", "admin listener base URL (default: http://<gateway.admin_listen>)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	timeout := fs.Duration("timeout", 5*time.Second, "how long to wait on the admin listener")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx := context.Background()
	cfg, db, err := openStore(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	base := *adminURL
	if base == "" {
		base = adminBaseURL(cfg.Gateway.AdminListen)
	}

	rep := buildStatus(ctx, cfg, db, base, *timeout)

	if *asJSON {
		if err := writeJSONTo(os.Stdout, rep); err != nil {
			return err
		}
	} else {
		rep.print(os.Stdout)
	}
	if len(rep.Problems) > 0 {
		return silentExit(1)
	}
	return nil
}

// statusReport is the `status` screen.
type statusReport struct {
	Gateway       gatewayLine        `json:"gateway"`
	Installations []installationLine `json:"installations"`
	WorkflowCache workflowCacheLine  `json:"workflow_cache"`
	Triggers      triggerLine        `json:"triggers"`
	Correlation   map[string]int64   `json:"correlation"`
	Coverage      coverageLine       `json:"coverage"`
	Problems      []string           `json:"problems"`
	Notes         []string           `json:"notes,omitempty"`
	WindowSeconds int64              `json:"window_seconds"`
	Thresholds    map[string]any     `json:"thresholds"`
}

type gatewayLine struct {
	Reachable     bool   `json:"reachable"`
	AdminURL      string `json:"admin_url"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	SQLiteOK      bool   `json:"sqlite_ok"`
	SchemaVersion int    `json:"schema_version"`
	FreeBytes     uint64 `json:"disk_free_bytes"`
	Unreachable   string `json:"unreachable_reason,omitempty"`
}

type installationLine struct {
	Bucket        string `json:"bucket"`
	Remaining     int64  `json:"rate_remaining"`
	ResetAt       int64  `json:"rate_reset_at"`
	ResetInSecond int64  `json:"resets_in_seconds"`
	Low           bool   `json:"low"`
	PausedTotal   int64  `json:"quota_paused_total"`
}

type workflowCacheLine struct {
	Warm  int      `json:"warm"`
	Stale int      `json:"stale"`
	Cold  int      `json:"cold"`
	Names []string `json:"cold_workflows,omitempty"`
}

type triggerLine struct {
	Forwarded            int64 `json:"forwarded"`
	Rejected             int64 `json:"rejected"`
	RejectedSuppressed   int64 `json:"rejected_suppressed"`
	Errored              int64 `json:"errored"`
	Unresolved           int64 `json:"unresolved"`
	UnresolvedPastHoriz  int64 `json:"unresolved_past_horizon"`
	OldestUnresolvedSecs int64 `json:"oldest_unresolved_seconds"`
}

type coverageLine struct {
	Covered    int64   `json:"covered_runs"`
	Unknown    int64   `json:"unknown_runs"`
	Gap        int64   `json:"gap_runs"`
	Total      int64   `json:"total_runs"`
	GapPct     float64 `json:"gap_pct"`
	UnknownPct float64 `json:"unknown_pct"`
	Migratable int64   `json:"gap_migratable_runs"`
	Structural int64   `json:"gap_structural_runs"`
	HistoryDay int     `json:"history_days"`
}

func buildStatus(ctx context.Context, cfg *config.Config, db *store.DB, adminURL string, timeout time.Duration) *statusReport {
	now := nowFunc()
	rep := &statusReport{
		Correlation:   map[string]int64{},
		WindowSeconds: int64(statusWindow.Seconds()),
		Thresholds: map[string]any{
			"quota_low_remaining":        statusQuotaLow,
			"resolver_horizon_seconds":   int64(statusResolverHorizon.Seconds()),
			"workflow_cache_ttl_seconds": int64(statusWorkflowCacheTTL.Seconds()),
			"configurable":               false,
			"why":                        "thresholds are deliberately not configurable in Phase 0; a configurable threshold is the first piece of Phase 1's policy surface",
		},
	}
	rep.Gateway.AdminURL = adminURL

	// ---- the running process's gauges (/metrics, /healthz)
	health, metricLines, err := fetchAdmin(ctx, adminURL, timeout)
	if err != nil {
		// Not fatal and not silent. The durable half of the report is still
		// produced from SQLite, and the operator is told exactly which half is
		// missing rather than shown a screen that looks complete.
		rep.Gateway.Unreachable = err.Error()
		rep.Problems = append(rep.Problems,
			fmt.Sprintf("admin listener %s is not answering: %v — quota and uptime could not be read", adminURL, err))
	} else {
		rep.Gateway.Reachable = true
		rep.Gateway.Version = health.Version
		rep.Gateway.UptimeSeconds = health.UptimeSeconds
		rep.Gateway.SQLiteOK = health.SQLiteOK
		rep.Gateway.SchemaVersion = health.SchemaVersion
		if !health.SQLiteOK {
			rep.Problems = append(rep.Problems, "the gateway reports sqlite_ok: false — quick_check failed, or free space is below storage.min_free_bytes")
		}
		rep.Installations = installationsFrom(metricLines, now)
		for _, inst := range rep.Installations {
			if inst.Low {
				rep.Problems = append(rep.Problems, fmt.Sprintf(
					"quota bucket %s is at %d, below %d: the rate-limit guard pauses BACKGROUND inventory work here, so the coverage number stops advancing while live dispatch keeps working",
					inst.Bucket, inst.Remaining, statusQuotaLow))
			}
		}
	}

	if free, err := store.FreeBytes(filepath.Dir(cfg.Storage.SQLitePath)); err == nil {
		rep.Gateway.FreeBytes = free
		if cfg.Storage.MinFreeBytes > 0 && free < cfg.Storage.MinFreeBytes {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"free space %s is below storage.min_free_bytes %s: trigger requests are answering 503 storage_degraded",
				humanBytes(free), humanBytes(cfg.Storage.MinFreeBytes)))
		}
	}

	// ---- workflow cache
	cacheLine, coldNames := workflowCacheState(ctx, cfg, db, now)
	rep.WorkflowCache = cacheLine
	if cacheLine.Cold > 0 {
		rep.Problems = append(rep.Problems, fmt.Sprintf(
			"%d workflow(s) COLD (%s): input validation is SKIPPED for them and the event records inputs_validation `%s` — never an implicit pass",
			cacheLine.Cold, strings.Join(coldNames, ", "), store.ValidationSkippedColdCache))
	}

	// ---- triggers and correlation
	tl, corr, err := triggerState(ctx, db, now)
	if err != nil {
		rep.Problems = append(rep.Problems, fmt.Sprintf("event log scan failed: %v", err))
	} else {
		rep.Triggers = tl
		rep.Correlation = corr
		if tl.UnresolvedPastHoriz > 0 {
			rep.Problems = append(rep.Problems, fmt.Sprintf(
				"%d forwarded trigger(s) have no run_link at all and are past the %s resolver horizon: the correlation sweep is not running, or it is failing — check gateway_correlation_sweep_error_total",
				tl.UnresolvedPastHoriz, statusResolverHorizon))
		}
	}

	// ---- coverage
	rows, err := db.Coverage(ctx, cfg.Inventory.HistoryDays)
	if err != nil {
		rep.Problems = append(rep.Problems, fmt.Sprintf("coverage query failed: %v", err))
	} else {
		s := store.Summarise(rows)
		rep.Coverage = coverageLine{
			Covered: s.CoveredCount, Unknown: s.UnknownCount, Gap: s.GapCount,
			Total: s.Total, GapPct: s.GapPct, UnknownPct: s.UnknownPct,
			HistoryDay: cfg.Inventory.HistoryDays,
		}
		for _, r := range rows {
			if r.CoverageState != store.CoverageGap {
				continue
			}
			switch r.GapClass {
			case store.GapMigratable:
				rep.Coverage.Migratable += r.RunCount
			case store.GapStructural:
				rep.Coverage.Structural += r.RunCount
			}
		}
	}

	// A non-zero gap is NOT a problem. The structural half never
	// converges to zero and reporting it as a fault would train operators to
	// ignore the one number the phase is measured by.
	rep.Notes = append(rep.Notes,
		"a non-zero coverage gap is expected: the `structural` half (UI runs, repository_dispatch, accepted direct calls) never converges to zero")

	return rep
}

// adminHealthBody is the subset of the /healthz body `status` reads.
type adminHealthBody struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	SQLiteOK      bool   `json:"sqlite_ok"`
	SchemaVersion int    `json:"schema_version"`
}

// fetchAdmin reads the admin /healthz and /metrics.
//
// Neither needs a token: the admin listener leaves /metrics and /healthz to network access
// control and requires a token only for /inventory. `status` therefore works
// from the box the gateway runs on with no credential, which is the point of
// a command an operator reaches for during an incident.
func fetchAdmin(ctx context.Context, base string, timeout time.Duration) (adminHealthBody, map[string][]metricSample, error) {
	var health adminHealthBody
	cl := &http.Client{Timeout: timeout}

	body, err := httpGet(ctx, cl, base+"/healthz")
	if err != nil {
		return health, nil, err
	}
	// A 503 body is still a valid /healthz document and carries sqlite_ok:
	// false, which is exactly the fact worth reporting.
	if err := json.Unmarshal(body, &health); err != nil {
		return health, nil, fmt.Errorf("admin /healthz returned a body that is not /healthz JSON: %w", err)
	}

	metricsBody, err := httpGet(ctx, cl, base+"/metrics")
	if err != nil {
		// Health answered, metrics did not: report health and no quota rather
		// than dropping both.
		return health, nil, nil
	}
	return health, parsePromText(string(metricsBody)), nil
}

func httpGet(ctx context.Context, cl *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	// 1 MiB is far above a Phase 0 /metrics page and stops a misconfigured
	// --admin-url pointing at something enormous from being read into memory.
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// metricSample is one series of the Prometheus text exposition.
type metricSample struct {
	Labels map[string]string
	Value  float64
}

// parsePromText reads the subset of the exposition format internal/metrics
// emits: one `name{k="v"} value` line per series, no timestamps, no exemplars.
// Histogram lines are read as ordinary series and ignored by name.
func parsePromText(s string) map[string][]metricSample {
	out := map[string][]metricSample{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sp := strings.LastIndex(line, " ")
		if sp < 0 {
			continue
		}
		value, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			continue
		}
		head := line[:sp]
		name, labels := head, map[string]string{}
		if brace := strings.IndexByte(head, '{'); brace >= 0 && strings.HasSuffix(head, "}") {
			name = head[:brace]
			for _, pair := range splitLabels(head[brace+1 : len(head)-1]) {
				eq := strings.IndexByte(pair, '=')
				if eq < 0 {
					continue
				}
				labels[pair[:eq]] = strings.Trim(pair[eq+1:], `"`)
			}
		}
		out[name] = append(out[name], metricSample{Labels: labels, Value: value})
	}
	return out
}

// splitLabels splits on commas that are not inside a quoted value.
func splitLabels(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && inQuote && i+1 < len(s):
			cur.WriteByte(c)
			i++
			cur.WriteByte(s[i])
		case c == '"':
			inQuote = !inQuote
			cur.WriteByte(c)
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// installationsFrom turns the per-bucket quota gauges into `status`'s
// INSTALLATIONS block.
//
// The label is `bucket`, not `installation_id`, for a reason: the App JWT bucket has no installation id, so an
// `installation_id` label could never publish it. The value set is `app` plus
// one key per installation.
func installationsFrom(m map[string][]metricSample, now time.Time) []installationLine {
	remaining := m["gateway_github_rate_remaining"]
	resets := map[string]float64{}
	for _, s := range m["gateway_github_rate_reset_timestamp"] {
		resets[s.Labels["bucket"]] = s.Value
	}
	paused := map[string]float64{}
	for _, s := range m["gateway_github_quota_paused_total"] {
		paused[s.Labels["bucket"]] = s.Value
	}

	out := make([]installationLine, 0, len(remaining))
	for _, s := range remaining {
		bucket := s.Labels["bucket"]
		line := installationLine{
			Bucket:      bucket,
			Remaining:   int64(s.Value),
			PausedTotal: int64(paused[bucket]),
		}
		if r, ok := resets[bucket]; ok {
			line.ResetAt = int64(r)
			line.ResetInSecond = line.ResetAt - now.Unix()
		}
		// The App bucket is never paused, because pausing token minting
		// would pause the calls needed to recover. Flagging it LOW would ask
		// the operator to act on something the gateway deliberately ignores.
		line.Low = bucket != forge.BucketApp && line.Remaining < statusQuotaLow
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out
}

// workflowCacheState classifies every configured target against the persistent
// workflow cache.
//
// The three states are not cosmetic: a STALE entry is
// still used and refreshed in the background, while a COLD one means the
// definition was never readable and input validation is skipped. Collapsing
// stale into cold would report a healthy gateway as broken; collapsing cold
// into stale would hide the one state that changes what the request path does.
func workflowCacheState(ctx context.Context, cfg *config.Config, db *store.DB, now time.Time) (workflowCacheLine, []string) {
	var line workflowCacheLine
	entries, err := db.WorkflowCacheAll(ctx)
	if err != nil {
		return line, nil
	}
	type key struct{ repo, workflow string }
	byKey := map[key]store.WorkflowCacheEntry{}
	for _, e := range entries {
		k := key{e.Repo, e.Workflow}
		if prev, ok := byKey[k]; ok && prev.Ref == forge.WorkflowCacheDefaultRef {
			continue
		}
		byKey[k] = e
	}

	var cold []string
	for _, target := range cfg.AllowedTargets {
		for _, wf := range target.Workflows {
			e, ok := byKey[key{target.Repo, wf}]
			switch {
			case !ok:
				// Never fetched: no row at all. Bootstrap has not reached this
				// target, or it failed (which never blocks startup).
				line.Cold++
				cold = append(cold, target.Repo+" "+wf+" (never fetched)")
			case e.InputsDef == "":
				// A row whose inputs_def is SQL NULL: the file could not be
				// read. Not the same as '{}' — that means "declares no inputs"
				// and is a complete definition.
				line.Cold++
				cold = append(cold, target.Repo+" "+wf+" (definition unreadable)")
			case now.Sub(e.FetchedAt) > statusWorkflowCacheTTL:
				line.Stale++
			default:
				line.Warm++
			}
		}
	}
	sort.Strings(cold)
	line.Names = cold
	return line, cold
}

// triggerState reads the TRIGGERS and CORRELATION blocks off the event log.
//
// `unresolved` here means a FORWARDED trigger with no active run_link at all —
// the resolver has not spoken yet. A trigger that has an active link with
// resolution `none`, `inferred` or `ambiguous` is NOT counted: those are
// settled answers, and the coverage query already reports them as coverage `unknown`.
// Counting them here would leave `status` permanently red over one old
// unresolvable trigger, and a check that is always red is a check nobody reads.
func triggerState(ctx context.Context, db *store.DB, now time.Time) (triggerLine, map[string]int64, error) {
	var line triggerLine
	corr := map[string]int64{}

	type triggerState struct {
		ts        time.Time
		forwarded bool
		linked    bool
	}
	triggers := map[string]*triggerState{}

	since := now.Add(-statusWindow)
	err := scanEvents(ctx, db, since, func(ev eventObj) error {
		switch ev.eventType() {
		case string(store.EventTrigger):
			triggers[ev.eventID()] = &triggerState{ts: ev.ts()}
			corr[orDash(ev.str("correlation"))]++
		case string(store.EventTriggerOutcome):
			switch store.OutcomeStatus(ev.str("outcome_status")) {
			case store.OutcomeForwarded:
				line.Forwarded++
				if t, ok := triggers[ev.parent()]; ok {
					t.forwarded = true
				}
			case store.OutcomeError:
				line.Errored++
			case store.OutcomeRejected:
				line.Rejected++
			}
		case string(store.EventTriggerRejected):
			line.Rejected++
			if n, ok := ev.num("suppressed_count"); ok {
				// The rejection path coalesces a 429 window into one row. The row count and
				// the request count are different numbers and the difference
				// is the whole reason gateway_trigger_rejected_total exists.
				line.RejectedSuppressed += n
			}
		case string(store.EventRunLink):
			if ev.str("superseded_by") == "" {
				if t, ok := triggers[ev.parent()]; ok {
					t.linked = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return line, corr, err
	}

	var oldest time.Time
	for _, t := range triggers {
		if !t.forwarded || t.linked {
			continue
		}
		line.Unresolved++
		if now.Sub(t.ts) > statusResolverHorizon {
			line.UnresolvedPastHoriz++
		}
		if oldest.IsZero() || t.ts.Before(oldest) {
			oldest = t.ts
		}
	}
	if !oldest.IsZero() {
		line.OldestUnresolvedSecs = int64(now.Sub(oldest).Seconds())
	}
	return line, corr, nil
}

func (r *statusReport) print(w io.Writer) {
	// ---- GATEWAY
	if r.Gateway.Reachable {
		sqlite := "sqlite ok"
		if !r.Gateway.SQLiteOK {
			sqlite = "sqlite DEGRADED"
		}
		fmt.Fprintf(w, "GATEWAY    up %s · v%s · %s · schema v%d · disk %s free\n",
			humanDuration(time.Duration(r.Gateway.UptimeSeconds)*time.Second),
			r.Gateway.Version, sqlite, r.Gateway.SchemaVersion, humanBytes(r.Gateway.FreeBytes))
	} else {
		fmt.Fprintf(w, "GATEWAY    NOT REACHABLE at %s · disk %s free\n", r.Gateway.AdminURL, humanBytes(r.Gateway.FreeBytes))
		fmt.Fprintf(w, "           %s\n", r.Gateway.Unreachable)
		fmt.Fprintf(w, "           Everything below comes from SQLite and is still accurate; quota and uptime are not shown\n")
		fmt.Fprintf(w, "           because they live in the running process and this build does not guess them.\n")
	}

	// ---- INSTALLATIONS
	fmt.Fprintf(w, "INSTALLATIONS\n")
	if len(r.Installations) == 0 {
		fmt.Fprintf(w, "  (no quota observed yet — no GitHub call has been made since the gateway started)\n")
	}
	for _, inst := range r.Installations {
		state := "ok"
		if inst.Low {
			state = "LOW"
		}
		reset := "-"
		if inst.ResetAt > 0 {
			reset = "resets in " + humanDuration(time.Duration(inst.ResetInSecond)*time.Second)
		}
		fmt.Fprintf(w, "  %-28s quota %5d   %-18s %s", inst.Bucket, inst.Remaining, reset, state)
		if inst.PausedTotal > 0 {
			fmt.Fprintf(w, "   (paused %d× — background inventory stopped)", inst.PausedTotal)
		}
		fmt.Fprintln(w)
	}

	// ---- WORKFLOW CACHE
	c := r.WorkflowCache
	fmt.Fprintf(w, "WORKFLOW CACHE\n")
	fmt.Fprintf(w, "  %d warm · %d stale (revalidating, still used) · %d COLD\n", c.Warm, c.Stale, c.Cold)
	for _, name := range c.Names {
		fmt.Fprintf(w, "    → %s\n", name)
	}
	if c.Cold > 0 {
		fmt.Fprintf(w, "    a COLD workflow means input validation is SKIPPED and the event records\n")
		fmt.Fprintf(w, "    inputs_validation `%s` — visible, never an implicit pass\n", store.ValidationSkippedColdCache)
	}

	// ---- TRIGGERS
	t := r.Triggers
	fmt.Fprintf(w, "TRIGGERS (last %s)\n", humanDuration(time.Duration(r.WindowSeconds)*time.Second))
	fmt.Fprintf(w, "  %d forwarded · %d rejected · %d errored · %d unresolved",
		t.Forwarded, t.Rejected, t.Errored, t.Unresolved)
	switch {
	case t.Unresolved == 0:
	case t.UnresolvedPastHoriz > 0:
		fmt.Fprintf(w, " (%d PAST THE %s HORIZON)", t.UnresolvedPastHoriz, statusResolverHorizon)
	default:
		fmt.Fprintf(w, " (oldest %s, within horizon)", humanDuration(time.Duration(t.OldestUnresolvedSecs)*time.Second))
	}
	fmt.Fprintln(w)
	if t.RejectedSuppressed > 0 {
		fmt.Fprintf(w, "  %d further request(s) were coalesced into those rejection rows; the row count\n", t.RejectedSuppressed)
		fmt.Fprintf(w, "  and the request count are different numbers and gateway_trigger_rejected_total holds the second\n")
	}

	// ---- CORRELATION
	fmt.Fprintf(w, "CORRELATION\n")
	if len(r.Correlation) == 0 {
		fmt.Fprintf(w, "  (no triggers in the window)\n")
	} else {
		var total int64
		keys := make([]string, 0, len(r.Correlation))
		for k, v := range r.Correlation {
			total += v
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", k, float64(r.Correlation[k])*100/float64(total)))
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, "  ·  "))
		fmt.Fprintf(w, "  (this is what each dispatch was ARMED with, before it was sent — not which tier\n")
		fmt.Fprintf(w, "   established the link. That is run_link.resolution_source.)\n")
	}

	// ---- COVERAGE
	cv := r.Coverage
	fmt.Fprintf(w, "COVERAGE (%dd, %d runs)\n", cv.HistoryDay, cv.Total)
	if cv.Total == 0 {
		fmt.Fprintf(w, "  (no runs in the window — bootstrap has not populated github_runs yet)\n")
	} else {
		fmt.Fprintf(w, "  covered %.1f%%  ·  unknown %.1f%%  ·  gap %.1f%%  (migratable %d / structural %d runs)\n",
			float64(cv.Covered)*100/float64(cv.Total), cv.UnknownPct, cv.GapPct, cv.Migratable, cv.Structural)
	}

	// ---- verdict
	fmt.Fprintln(w)
	if len(r.Problems) == 0 {
		fmt.Fprintf(w, "OK — cold cache 0, quota above %d, no unresolved trigger past the %s horizon. exit 0\n",
			statusQuotaLow, statusResolverHorizon)
	} else {
		fmt.Fprintf(w, "PROBLEMS (exit 1)\n")
		for _, p := range r.Problems {
			fmt.Fprintf(w, "  ! %s\n", p)
		}
	}
	for _, n := range r.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
	fmt.Fprintf(w, "  thresholds are fixed in Phase 0 and not configurable.\n")
}

// adminBaseURL turns gateway.admin_listen into a URL. A bare ":9090" binds all
// interfaces; the loopback address is what an operator on the box can reach,
// and the admin listener belongs on localhost anyway.
func adminBaseURL(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "http://127.0.0.1" + listen
	}
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "http://127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	}
	return "http://" + listen
}
