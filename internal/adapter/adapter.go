// Package adapter is the composition-root translation between internal/store
// and internal/forge.
//
// Neither of those packages imports the other. Each declares its own types for
// the same facts, and this package is the single place where one becomes the
// other. A signature change on either side is a
// compile error HERE, which is where it should surface.
//
// # Why every conversion is a switch that can fail
//
// There are two known mistakes that compile, pass vet, and write a
// legal enum value with no external symptom. Both are shaped the same way: a
// zero value in a closed enum is not "unknown", it is a specific, wrong,
// perfectly valid answer.
//
//   - A zero forge.PlanOptions reads as "tier 1 off" and writes correlation
//     `window` on a deployment where tier 1 is on. This package never
//     constructs PlanOptions — the request path calls forge's own Plan, which
//     fills it from the client's resolved configuration, so the hazard has no
//     call site to occur at.
//   - A dropped PendingTrigger.Correlation reads as "tier 2 not armed" and
//     routes every injected trigger to the time-window path, downgrading
//     `exact` links to `inferred` or `ambiguous`. That one IS this package's
//     to get right, and pendingTriggers below is where.
//
// So no conversion here uses a `default:` arm that returns a zero value. An
// unrecognised value is an ERROR. A closed enum with a CHECK constraint behind
// it cannot legally produce one; if it ever does, the loud failure is correct
// and the quiet substitution is not: silent degradation is the first failure
// mode this project guards against.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// --- enum conversions -------------------------------------------------------

// Correlation converts the correlation armament label, forge -> store.
func Correlation(c forge.Correlation) (store.Correlation, error) {
	switch c {
	case forge.CorrelationInjected:
		return store.CorrelationInjected, nil
	case forge.CorrelationDispatchResponse:
		return store.CorrelationDispatchResponse, nil
	case forge.CorrelationWindow:
		return store.CorrelationWindow, nil
	case "":
		// The zero value is the hazard itself. It is NOT silently read as
		// `window`: a trigger event whose correlation was never decided is a
		// bug in the request path, and the schema's CHECK would accept `window`
		// without complaint.
		return "", errors.New("adapter: correlation is empty; it is required on every trigger event")
	default:
		return "", fmt.Errorf("adapter: unknown forge correlation %q", c)
	}
}

// StoreCorrelation converts the same label store -> forge. It is what makes
// the resolver's tier-2 branch reachable: the resolver branches on it, so an
// UnresolvedTrigger that dropped it would never take that branch.
func StoreCorrelation(c store.Correlation) (forge.Correlation, error) {
	switch c {
	case store.CorrelationInjected:
		return forge.CorrelationInjected, nil
	case store.CorrelationDispatchResponse:
		return forge.CorrelationDispatchResponse, nil
	case store.CorrelationWindow:
		return forge.CorrelationWindow, nil
	default:
		return "", fmt.Errorf("adapter: unknown store correlation %q", c)
	}
}

// InputsValidation converts the inputs_validation closed enum, forge -> store.
func InputsValidation(v forge.InputsValidation) (store.InputsValidation, error) {
	switch v {
	case forge.InputsValidationDefaultBranch:
		return store.ValidationDefaultBranch, nil
	case forge.InputsValidationExactRef:
		return store.ValidationExactRef, nil
	case forge.InputsValidationSkippedColdCache:
		return store.ValidationSkippedColdCache, nil
	case forge.InputsValidationSkippedNonDefaultRef:
		return store.ValidationSkippedNonDefaultRef, nil
	default:
		return "", fmt.Errorf("adapter: unknown forge inputs validation %q", v)
	}
}

// Resolution converts run_link's reliability class, forge -> store.
func Resolution(r forge.Resolution) (store.Resolution, error) {
	switch r {
	case forge.ResolutionExact:
		return store.ResolutionExact, nil
	case forge.ResolutionInferred:
		return store.ResolutionInferred, nil
	case forge.ResolutionAmbiguous:
		return store.ResolutionAmbiguous, nil
	case forge.ResolutionNone:
		return store.ResolutionNone, nil
	default:
		return "", fmt.Errorf("adapter: unknown forge resolution %q", r)
	}
}

// StoreResolution converts store -> forge. The empty value is legal here and
// means "this trigger has no active link yet" (PendingTrigger's contract).
func StoreResolution(r store.Resolution) (forge.Resolution, error) {
	switch r {
	case "":
		return "", nil
	case store.ResolutionExact:
		return forge.ResolutionExact, nil
	case store.ResolutionInferred:
		return forge.ResolutionInferred, nil
	case store.ResolutionAmbiguous:
		return forge.ResolutionAmbiguous, nil
	case store.ResolutionNone:
		return forge.ResolutionNone, nil
	default:
		return "", fmt.Errorf("adapter: unknown store resolution %q", r)
	}
}

// ResolutionSource converts run_link's mandatory source, forge -> store.
func ResolutionSource(s forge.ResolutionSource) (store.ResolutionSource, error) {
	switch s {
	case forge.ResolutionSourceDispatchResponse:
		return store.SourceDispatchResponse, nil
	case forge.ResolutionSourceRunName:
		return store.SourceRunName, nil
	case forge.ResolutionSourceWindow:
		return store.SourceWindow, nil
	default:
		// The schema has a table-level CHECK: the zero value is not a legal column
		// value on any run_link.
		return "", fmt.Errorf("adapter: unknown forge resolution source %q", s)
	}
}

// RunLinkEvent converts a forge.RunLink into the store's event.
//
// It is the ONLY conversion of a run_link in this codebase, and the tier-1
// link that reaches it is always built by forge.RunLinkFromDispatch — never by
// a literal at the call site — so `dispatch_response` is never spelled outside
// internal/forge (see the zero-value hazards in the package comment).
func RunLinkEvent(l forge.RunLink) (store.RunLinkEvent, error) {
	res, err := Resolution(l.Resolution)
	if err != nil {
		return store.RunLinkEvent{}, err
	}
	src, err := ResolutionSource(l.ResolutionSource)
	if err != nil {
		return store.RunLinkEvent{}, err
	}
	ev := store.RunLinkEvent{
		TriggerEventID:   l.TriggerEventID,
		Resolution:       res,
		ResolutionSource: src,
		TS:               l.At,
	}
	if l.RunID != 0 {
		id := l.RunID
		ev.RunID = &id
	} else if res != store.ResolutionNone {
		// Only `none` may carry no run id. A missing id on any other
		// resolution would write a link that claims a match it cannot name.
		return store.RunLinkEvent{}, fmt.Errorf("adapter: run_link with resolution %q has no run id", res)
	}
	return ev, nil
}

// --- forge.Store over *store.DB --------------------------------------------

// StoreDB is the subset of *store.DB this adapter needs. Naming it as an
// interface keeps the adapter testable without a database.
type StoreDB interface {
	WorkflowDefinition(ctx context.Context, repo, workflow, ref string) (store.WorkflowCacheEntry, error)
	UpsertWorkflowCache(ctx context.Context, entries []store.WorkflowCacheEntry) error
	UpsertGitHubRuns(ctx context.Context, runs []store.GitHubRun) ([]store.ConclusionChange, error)
	UnresolvedTriggers(ctx context.Context, horizon time.Duration, limit int) ([]store.UnresolvedTrigger, error)
	AppendRunLink(ctx context.Context, ev store.RunLinkEvent) (string, error)
	AppendRunObserved(ctx context.Context, ev store.RunObservedEvent) (string, error)
}

// ForgeStore implements forge.Store on top of the SQLite store.
type ForgeStore struct {
	DB StoreDB
}

var _ forge.Store = (*ForgeStore)(nil)

// GetWorkflowCache implements forge.WorkflowCacheStore.
//
// ok=false means NEVER FETCHED, and only that. A stale row comes back with
// ok=true because the workflow cache is stale-while-revalidate: reporting stale
// as cold would make every request skip input validation, which is silent
// degradation and the number one failure class here.
func (f *ForgeStore) GetWorkflowCache(ctx context.Context, repo, workflow, ref string) (forge.WorkflowCacheEntry, bool, error) {
	row, err := f.DB.WorkflowDefinition(ctx, repo, workflow, ref)
	if errors.Is(err, store.ErrNotFound) {
		return forge.WorkflowCacheEntry{}, false, nil
	}
	if err != nil {
		return forge.WorkflowCacheEntry{}, false, err
	}
	entry, err := WorkflowEntry(row)
	if err != nil {
		return forge.WorkflowCacheEntry{}, false, err
	}
	return entry, true, nil
}

// WorkflowEntry converts a cache row, store -> forge.
//
// The nil-versus-empty distinction on InputsDef is load-bearing, and it is an
// invariant that cannot be read off a signature:
//
//	""    -> nil map          the workflow file was never read; input validation
//	                          is SKIPPED and the event says skipped_cold_cache.
//	"{}"  -> empty non-nil    the file was read and declares no inputs, so any
//	                          input key the caller sends is a 422.
//
// Collapsing them makes the gateway either skip validation it should perform
// or reject a legitimate request, and neither shows up as an error.
func WorkflowEntry(row store.WorkflowCacheEntry) (forge.WorkflowCacheEntry, error) {
	entry := forge.WorkflowCacheEntry{
		Repo:          row.Repo,
		Workflow:      row.Workflow,
		Ref:           row.Ref,
		WorkflowID:    row.WorkflowID,
		Triggers:      row.Triggers,
		DefaultBranch: row.DefaultBranch,
		FetchedAt:     row.FetchedAt,
	}
	if row.InputsDef == "" {
		entry.InputsDef = nil
		return entry, nil
	}
	defs := map[string]forge.InputDef{}
	if err := json.Unmarshal([]byte(row.InputsDef), &defs); err != nil {
		return forge.WorkflowCacheEntry{}, fmt.Errorf("adapter: workflow_cache.inputs_def for %s/%s: %w", row.Repo, row.Workflow, err)
	}
	entry.InputsDef = defs
	return entry, nil
}

// PutWorkflowCache implements forge.WorkflowCacheStore.
func (f *ForgeStore) PutWorkflowCache(ctx context.Context, entry forge.WorkflowCacheEntry) error {
	row, err := WorkflowRow(entry)
	if err != nil {
		return err
	}
	return f.DB.UpsertWorkflowCache(ctx, []store.WorkflowCacheEntry{row})
}

// WorkflowRow converts a cache entry, forge -> store. It is the other half of
// WorkflowEntry's nil-versus-empty rule.
func WorkflowRow(entry forge.WorkflowCacheEntry) (store.WorkflowCacheEntry, error) {
	row := store.WorkflowCacheEntry{
		Repo:          entry.Repo,
		Workflow:      entry.Workflow,
		Ref:           entry.Ref,
		WorkflowID:    entry.WorkflowID,
		Triggers:      entry.Triggers,
		DefaultBranch: entry.DefaultBranch,
		FetchedAt:     entry.FetchedAt,
	}
	if entry.InputsDef == nil {
		// Unknown definition: stored as SQL NULL, NOT as '{}'.
		row.InputsDef = ""
		return row, nil
	}
	buf, err := json.Marshal(entry.InputsDef)
	if err != nil {
		return store.WorkflowCacheEntry{}, err
	}
	row.InputsDef = string(buf)
	return row, nil
}

// UpsertRuns implements forge.RunHistoryStore.
//
// The store reports conclusion changes as ConclusionChange rows; forge wants
// the RunRecords they belong to, so the run_observed producer does not have
// to re-read. The mapping back is by run id against the batch that was just
// written.
func (f *ForgeStore) UpsertRuns(ctx context.Context, runs []forge.RunRecord) ([]forge.RunRecord, error) {
	rows := make([]store.GitHubRun, 0, len(runs))
	byID := make(map[int64]forge.RunRecord, len(runs))
	for _, r := range runs {
		byID[r.RunID] = r
		rows = append(rows, store.GitHubRun{
			RunID:                r.RunID,
			Repo:                 r.Repo,
			WorkflowID:           r.WorkflowID,
			Workflow:             r.Workflow,
			Event:                r.Event,
			HeadBranch:           r.HeadBranch,
			HeadBranchNormalized: r.HeadBranchNormalized,
			Actor:                r.Actor,
			TriggeringActor:      r.TriggeringActor,
			Conclusion:           r.Conclusion,
			CreatedAt:            r.CreatedAt,
			FetchedAt:            r.FetchedAt,
		})
	}
	changes, err := f.DB.UpsertGitHubRuns(ctx, rows)
	if err != nil {
		return nil, err
	}
	out := make([]forge.RunRecord, 0, len(changes))
	for _, c := range changes {
		if rec, ok := byID[c.RunID]; ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// PendingCorrelations implements forge.CorrelationStore.
//
// THIS IS THE SECOND ZERO-VALUE HAZARD. Both Correlation and ExistingResolution
// must be carried across. Correlation especially: the resolver gates its
// tier-2 branch on Correlation.ArmsRunNameMatch(), a zero value there is not
// "unknown" but "tier 2 not armed", and every injected trigger would be sent
// down the time-window path and come back `inferred` or `ambiguous` instead of
// `exact`. Nothing errors, nothing logs, and the only visible effect is a
// coverage report that slowly stops saying `covered`.
func (f *ForgeStore) PendingCorrelations(ctx context.Context, horizon time.Duration, limit int) ([]forge.PendingTrigger, error) {
	rows, err := f.DB.UnresolvedTriggers(ctx, horizon, limit)
	if err != nil {
		return nil, err
	}
	out := make([]forge.PendingTrigger, 0, len(rows))
	for _, r := range rows {
		corr, err := StoreCorrelation(r.Correlation)
		if err != nil {
			return nil, fmt.Errorf("adapter: trigger %s: %w", r.EventID, err)
		}
		existing, err := StoreResolution(r.ExistingResolution)
		if err != nil {
			return nil, fmt.Errorf("adapter: trigger %s: %w", r.EventID, err)
		}
		out = append(out, forge.PendingTrigger{
			EventID:            r.EventID,
			Repo:               r.TargetProject,
			Workflow:           r.TargetWorkflow,
			Ref:                r.TargetRef,
			At:                 r.TS,
			Correlation:        corr,
			ExistingResolution: existing,
		})
	}
	return out, nil
}

// WriteRunLink implements forge.CorrelationStore.
//
// store.ErrWeakerResolution is swallowed: run_link's supersession rule lives
// INSIDE the store and is the authority; the resolver's own check is only an
// optimisation that two concurrent sweeps can race past. "A stronger link
// already exists" is the rule working, not a failure, and returning it as an
// error would make the resolver count a correct outcome as a sweep error.
func (f *ForgeStore) WriteRunLink(ctx context.Context, link forge.RunLink) error {
	ev, err := RunLinkEvent(link)
	if err != nil {
		return err
	}
	_, err = f.DB.AppendRunLink(ctx, ev)
	if errors.Is(err, store.ErrWeakerResolution) {
		return nil
	}
	return err
}

// WriteRunObserved implements forge.CorrelationStore.
func (f *ForgeStore) WriteRunObserved(ctx context.Context, o forge.RunObserved) error {
	_, err := f.DB.AppendRunObserved(ctx, store.RunObservedEvent{
		TS:             o.At,
		TriggerEventID: o.TriggerEventID,
		RunID:          o.RunID,
		Conclusion:     o.Conclusion,
	})
	return err
}
