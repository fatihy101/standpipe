package httpapi

import (
	"context"
	"time"

	"github.com/fatihy101/standpipe/internal/store"
)

// InventoryStore is the read surface /inventory needs. It is narrower than Store
// on purpose: `standpipe inventory` runs against a database with no HTTP
// server around it, so the report builder must not require one.
type InventoryStore interface {
	Coverage(ctx context.Context, historyDays int) ([]store.CoverageRow, error)
	Observed(ctx context.Context, historyDays int) ([]store.ObservedRow, error)
	Surface(ctx context.Context) ([]store.SurfaceRow, error)
}

// InventoryReport is the /inventory body, and it is exported for one reason: the
// admin endpoint and `standpipe inventory` must produce the SAME bytes. The
// delivery condition of Phase 0 is stated in terms of what
// `standpipe inventory` shows, so an operator reading the CLI and an operator
// reading /inventory cannot be allowed to see two different coverage numbers.
type InventoryReport struct {
	GeneratedAt int64           `json:"generated_at"`
	Surface     []SurfaceEntry  `json:"surface"`
	Observed    []ObservedEntry `json:"observed"`

	CoverageGapCount   int64   `json:"coverage_gap_count"`
	CoverageGapPct     float64 `json:"coverage_gap_pct"`
	CoverageUnknownCnt int64   `json:"coverage_unknown_count"`
	CoverageUnknownPct float64 `json:"coverage_unknown_pct"`

	// CoverageGapByClass is the migratable/structural split of the coverage
	// gap. It is not in the original example body, and it is here because the
	// split is the point: `migratable` is Phase 0's success measure and `structural` never
	// converges to zero. A single gap percentage invites reading the
	// structural half as a defect.
	CoverageGapByClass map[string]int64 `json:"coverage_gap_by_class"`

	// CoveredCount and TotalRuns are the denominator the percentages are
	// taken over. Without them a reader cannot tell 14.3% of seven runs from
	// 14.3% of seven thousand.
	CoveredCount int64 `json:"covered_count"`
	TotalRuns    int64 `json:"total_runs"`

	// Routing is present only when a routing.json path was given. The
	// gateway VALIDATES that file here and nowhere else; nothing in the
	// request path reads it.
	Routing *RoutingStatus `json:"routing,omitempty"`
}

// RoutingStatus is the routing.json validation result carried in the inventory body.
type RoutingStatus struct {
	Path   string   `json:"path"`
	Valid  bool     `json:"valid"`
	Errors []string `json:"errors,omitempty"`
}

// SurfaceEntry is one bootstrapped workflow.
type SurfaceEntry struct {
	Repo     string   `json:"repo"`
	Workflow string   `json:"workflow"`
	Triggers []string `json:"triggers"`
	// CorrelationReady: standby_trigger_id is declared in the cached
	// inputs definition. false is NOT a defect — tier 1 still produces
	// `exact` — it means the recovery path is not armed.
	CorrelationReady bool `json:"correlation_ready"`
}

// ObservedEntry is one row of the `observed` list.
type ObservedEntry struct {
	GitHubActor   *string `json:"github_actor"`
	Caller        *string `json:"caller"`
	CallerTokenID *string `json:"caller_token_id"`
	Workflow      string  `json:"workflow"`
	Repo          string  `json:"repo"`
	WeeklyCount   int64   `json:"weekly_count"`
	LastSeen      int64   `json:"last_seen"`
	Backend       *string `json:"backend"`
	Coverage      string  `json:"coverage"`
}

// BuildInventory assembles the /inventory report from three store reads.
//
// It takes the clock rather than calling time.Now so the endpoint and the CLI
// stamp generated_at the same way, and so a test can pin it.
func BuildInventory(ctx context.Context, st InventoryStore, historyDays int, now time.Time) (*InventoryReport, error) {
	surface, err := st.Surface(ctx)
	if err != nil {
		return nil, err
	}
	observed, err := st.Observed(ctx, historyDays)
	if err != nil {
		return nil, err
	}
	coverage, err := st.Coverage(ctx, historyDays)
	if err != nil {
		return nil, err
	}
	summary := store.Summarise(coverage)

	out := &InventoryReport{
		GeneratedAt: now.Unix(),
		Surface:     make([]SurfaceEntry, 0, len(surface)),
		Observed:    make([]ObservedEntry, 0, len(observed)),
		// The gap never converges to zero, and the two halves are
		// reported separately so the structural half does not read as a bug.
		CoverageGapByClass: map[string]int64{},
		CoverageGapCount:   summary.GapCount,
		CoverageGapPct:     summary.GapPct,
		CoverageUnknownCnt: summary.UnknownCount,
		// gap_pct and unknown_pct are separate gauges and are NEVER summed.
		CoverageUnknownPct: summary.UnknownPct,
		CoveredCount:       summary.CoveredCount,
		TotalRuns:          summary.Total,
	}
	for _, row := range coverage {
		if row.CoverageState == store.CoverageGap && row.GapClass != "" {
			out.CoverageGapByClass[row.GapClass] += row.RunCount
		}
	}
	// Both classes are always present, at zero if need be. An absent key reads
	// as "no structural gap measured yet", which is a different claim from
	// "the structural gap is zero", and only one of them is ever true.
	for _, class := range []string{store.GapMigratable, store.GapStructural} {
		if _, ok := out.CoverageGapByClass[class]; !ok {
			out.CoverageGapByClass[class] = 0
		}
	}
	for _, row := range surface {
		out.Surface = append(out.Surface, SurfaceEntry{
			Repo: row.Repo, Workflow: row.Workflow,
			Triggers: row.Triggers, CorrelationReady: row.CorrelationReady,
		})
	}
	for _, row := range observed {
		out.Observed = append(out.Observed, ObservedEntry{
			GitHubActor:   nullString(row.GitHubActor),
			Caller:        nullString(row.Caller),
			CallerTokenID: nullString(row.CallerTokenID),
			Workflow:      row.Workflow,
			Repo:          row.Repo,
			WeeklyCount:   row.WeeklyCount,
			LastSeen:      row.LastSeen.Unix(),
			Backend:       nullString(row.Backend),
			Coverage:      row.Coverage,
		})
	}
	return out, nil
}

// nullString renders an empty string as JSON null. The documented example uses null
// for "no caller" and "no backend", and an empty string there would read as a
// caller whose name is "".
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
