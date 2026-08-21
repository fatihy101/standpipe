package httpapi

import (
	"context"

	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// Store is what the HTTP surface needs from durable storage.
//
// It is a narrow view of store.Store rather than store.Store itself: this
// package writes no SQL, runs no background loop, and must not be able to
// reach the maintenance calls by accident. *store.DB satisfies it.
type Store interface {
	RejectStore

	// Reserve takes the idempotency key AND writes the trigger event in one
	// transaction (steps 14/14a).
	Reserve(ctx context.Context, req store.ReserveRequest) (store.ReserveResult, error)
	// MarkDispatchStarted must RETURN before the GitHub call is issued
	// (step 16): the crash sweep tells a queued request from a dispatched
	// one by that stamp.
	MarkDispatchStarted(ctx context.Context, callerName, key, eventID string) error
	// Complete writes the result, the trigger_outcome event and the optional
	// tier-1 run_link in ONE transaction (steps 16-17), so no state can occur
	// where the dispatch succeeded and the run id was lost.
	Complete(ctx context.Context, req store.CompleteRequest) (store.CompleteResult, error)

	// InputsHash computes the HMAC over the canonical inputs.
	InputsHash(ctx context.Context, canonicalInputs []byte) (string, error)
	// WorkflowDefinition backs the allowlist's default-branch check. ErrNotFound means
	// the definition was never fetched, which is 403 default_branch_unknown
	// and never an implicit allow.
	WorkflowDefinition(ctx context.Context, repo, workflow, ref string) (store.WorkflowCacheEntry, error)

	// CheckFreeSpace is the PROACTIVE free-space threshold. Its ErrStorageDegraded is
	// a 503; a write that actually fails is a 500. See storageError.
	CheckFreeSpace() error
	// HealthCheck is PRAGMA quick_check.
	HealthCheck(ctx context.Context) error
	SchemaVersion(ctx context.Context) (int, error)

	// Reads behind the admin /inventory endpoint.
	Coverage(ctx context.Context, historyDays int) ([]store.CoverageRow, error)
	Observed(ctx context.Context, historyDays int) ([]store.ObservedRow, error)
	Surface(ctx context.Context) ([]store.SurfaceRow, error)
}

// Dispatcher is what the HTTP surface needs from GitHub. *forge.Client
// satisfies it.
//
// Plan is used rather than forge.PlanDispatch on purpose. PlanDispatch takes a
// forge.PlanOptions, and a zero-valued one reads as "tier 1 off" and records
// correlation `window` on a deployment where tier 1 is on — an adapter hazard
// that produces valid-looking data and has no external symptom. Client.Plan fills PlanOptions
// from the client's own resolved configuration, so this package never
// constructs one and the hazard has no call site here.
type Dispatcher interface {
	Plan(ctx context.Context, req forge.DispatchRequest) (forge.DispatchPlan, error)
	Dispatch(ctx context.Context, req forge.DispatchRequest, plan forge.DispatchPlan) (*forge.DispatchResult, error)
	RateSnapshots() []forge.RateSnapshot
}
