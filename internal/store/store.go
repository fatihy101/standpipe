package store

import (
	"context"
	"io"
	"time"
)

// Store is the contract internal/httpapi, internal/forge and the CLI depend
// on. No package outside internal/store writes SQL; everything
// the rest of the gateway needs from durable storage is here.
//
// The interface is grouped by the request path, then by the background
// loops, then by operations.
//
// # Ordering rules that the type system cannot express
//
//  1. Reserve is called BEFORE any dispatch and its result decides whether a
//     dispatch may happen at all (ReserveOutcome.MayDispatch). On
//     ReserveInProgress the request is answered 409 and NOTHING is forwarded.
//  2. MarkDispatchStarted is called and has RETURNED before the GitHub request
//     is issued. The crash-recovery sweep distinguishes queued
//     from dispatched by that timestamp; stamping late makes the sweeper flip
//     live requests.
//  3. Complete is called with the same EventID that Reserve returned. A
//     CompleteResult with Applied == false means the record was taken over and
//     this attempt's response must not be presented as authoritative.
//  4. On ReserveReclaimed the run-link resolver runs against
//     ReserveResult.OriginalAttemptAt / OriginalDispatchAt BEFORE dispatching.
//     A matching run means Complete(StateComplete, forwarded) and no
//     second dispatch.
//  5. A tier-1 run_link (the dispatch response carried a workflow_run_id) is
//     passed to Complete as CompleteRequest.RunLink, NOT written with
//     AppendRunLink. It must share the completion's transaction, or the
//     dispatch can succeed while its run id is lost.
type Store interface {
	// --- request path ---

	// Reserve takes the idempotency key and writes the trigger event in the
	// same transaction.
	Reserve(ctx context.Context, req ReserveRequest) (ReserveResult, error)
	// MarkDispatchStarted stamps dispatch_started_at.
	MarkDispatchStarted(ctx context.Context, callerName, key, eventID string) error
	// Complete records the result and appends trigger_outcome.
	Complete(ctx context.Context, req CompleteRequest) (CompleteResult, error)
	// AppendTriggerRejected logs an authenticated request refused before any
	// reservation was taken as the parentless
	// trigger_rejected event. 429s arrive coalesced per caller per
	// reject_log_window.
	AppendTriggerRejected(ctx context.Context, ev TriggerRejectedEvent) (string, error)
	// LookupIdempotency reads one record, for diagnostics.
	LookupIdempotency(ctx context.Context, callerName, key string) (IdempotencyRecord, error)

	// InputsHash computes the trigger's inputs_hash with the persistent HMAC key,
	// prefixed by that key's random id. The key never leaves the store.
	InputsHash(ctx context.Context, canonicalInputs []byte) (string, error)
	// VerifyInputsHash re-checks a value. ErrUnknownHMACKey ("the key that
	// produced this is not in this database") is deliberately distinct from a
	// digest mismatch ("this record was tampered with").
	VerifyInputsHash(ctx context.Context, canonicalInputs []byte, value string) error
	// WorkflowDefinition reads the persistent workflow cache.
	// ErrNotFound means COLD — the caller must surface that, not hide it.
	WorkflowDefinition(ctx context.Context, repo, workflow, ref string) (WorkflowCacheEntry, error)

	// --- background loops ---

	// UnresolvedTriggers drives the query-based run_id resolver.
	UnresolvedTriggers(ctx context.Context, horizon time.Duration, limit int) ([]UnresolvedTrigger, error)
	// AppendRunLink applies the run_link supersede rule.
	AppendRunLink(ctx context.Context, ev RunLinkEvent) (string, error)
	// AppendRunObserved records a run conclusion.
	AppendRunObserved(ctx context.Context, ev RunObservedEvent) (string, error)
	// ActiveRunLink and GitHubRunConclusion let the resolver decide whether a
	// freshly written run_link should immediately emit run_observed.
	ActiveRunLink(ctx context.Context, runID int64) (string, error)
	GitHubRunConclusion(ctx context.Context, runID int64) (string, error)

	// UpsertWorkflowCache and UpsertGitHubRuns are the inventory refresh's
	// write side. UpsertGitHubRuns returns the conclusion changes
	// that must become run_observed events.
	UpsertWorkflowCache(ctx context.Context, entries []WorkflowCacheEntry) error
	UpsertGitHubRuns(ctx context.Context, runs []GitHubRun) ([]ConclusionChange, error)

	// SweepStaleInFlight, PurgeExpiredIdempotency, PurgeOldEvents and
	// PurgeOldRuns are the periodic cleanup goroutine.
	SweepStaleInFlight(ctx context.Context, p SweepParams) (int64, error)
	// PurgeExpiredIdempotency returns ErrClockJumpSuspected when it refused to
	// delete anything because of a suspected forward clock jump.
	// The caller logs it at warn and emits gateway_clock_jump_suspected_total
	// from ClockJumpSuspectedTotal.
	PurgeExpiredIdempotency(ctx context.Context) (int64, error)
	ClockJumpSuspectedTotal() int64
	PurgeOldEvents(ctx context.Context, retention time.Duration) (int64, error)
	PurgeOldRuns(ctx context.Context, retention time.Duration) (int64, error)

	// --- reads for inventory and the CLI ---

	Coverage(ctx context.Context, historyDays int) ([]CoverageRow, error)
	Observed(ctx context.Context, historyDays int) ([]ObservedRow, error)
	Surface(ctx context.Context) ([]SurfaceRow, error)
	WorkflowCacheAll(ctx context.Context) ([]WorkflowCacheEntry, error)
	ExportEvents(ctx context.Context, since time.Time, w io.Writer) (int, error)
	EventByID(ctx context.Context, eventID string) (map[string]any, error)
	// EventsByTriggerID reads one trigger's children — trigger_outcome,
	// run_link (active and superseded), run_observed — as an index seek
	// instead of a forward scan of the whole log.
	EventsByTriggerID(ctx context.Context, triggerEventID string) ([]map[string]any, error)
	// VerifyContentHash recomputes one event's content_sha256 from the stored
	// row over its closed hash scope. The answer is
	// three-valued: only IntegrityResult.Verdict.Verified() authorises the
	// claim that the row is intact, and "cannot verify" is a verdict with a
	// reason rather than an error or a mismatch.
	VerifyContentHash(ctx context.Context, eventID string) (IntegrityResult, error)

	// --- operations ---

	// HealthCheck is PRAGMA quick_check on the read pool.
	HealthCheck(ctx context.Context) error
	// CheckFreeSpace enforces storage.min_free_bytes. A non-nil
	// ErrStorageDegraded is a 503, not a 500.
	CheckFreeSpace() error
	// BackupTo takes a VACUUM INTO snapshot.
	BackupTo(ctx context.Context, path string) error
	// SchemaVersion is PRAGMA user_version.
	SchemaVersion(ctx context.Context) (int, error)
	// Forget NULLs the redactable fields.
	Forget(ctx context.Context, before time.Time) (int64, error)
	ForgetPlan(ctx context.Context, before time.Time) (int64, error)

	Close() error
}
