package store

import "time"

// State is the idempotency record state. All three are real states and behave
// differently; a retry from unknown is guarded by a CAS update.
type State string

const (
	// StateInFlight — a reservation is held and a dispatch may be in progress.
	// A second request for this key is answered 409 idempotency_in_progress
	// and is NEVER dispatched.
	StateInFlight State = "in_flight"
	// StateComplete — a deterministic result is recorded and replayable.
	StateComplete State = "complete"
	// StateUnknown — the dispatch timed out or upstream returned 5xx. The key
	// is retryable, but only through the reservation protocol's CAS.
	StateUnknown State = "unknown"
)

// EventType is the trigger log's polymorphic discriminator.
type EventType string

const (
	EventTrigger        EventType = "trigger"
	EventTriggerOutcome EventType = "trigger_outcome"
	// EventTriggerRejected is the parentless rejection event: an authenticated
	// request refused BEFORE any reservation was taken (rate limit, caller
	// policy, allowlist and request validation, all before the reservation). It
	// bears no idempotency, takes no reservation, and its trigger_event_id is
	// always NULL.
	EventTriggerRejected EventType = "trigger_rejected"
	EventRunLink         EventType = "run_link"
	EventRunObserved     EventType = "run_observed"
)

// OutcomeStatus is the closed enum of trigger_outcome.outcome_status.
type OutcomeStatus string

const (
	OutcomeForwarded OutcomeStatus = "forwarded"
	OutcomeRejected  OutcomeStatus = "rejected"
	OutcomeError     OutcomeStatus = "error"
)

// InputsValidation is the closed enum of trigger.inputs_validation. It carries
// the reason, not a boolean, because "validation was skipped" must never look
// like "validation passed": silent degradation must surface as a field.
type InputsValidation string

const (
	ValidationDefaultBranch        InputsValidation = "default_branch"
	ValidationExactRef             InputsValidation = "exact_ref"
	ValidationSkippedColdCache     InputsValidation = "skipped_cold_cache"
	ValidationSkippedNonDefaultRef InputsValidation = "skipped_non_default_ref"
)

// Correlation records which layer of the run_id correlation hierarchy this
// dispatch was ARMED with — not which layer eventually established the link
// (that is RunLinkEvent.ResolutionSource).
//
// It is decided when the reservation is taken, BEFORE the dispatch, and it is
// inside the content_sha256 hash scope: it is a claim about what the gateway did,
// and it cannot be derived from the log afterwards, because an inputs object
// that was not injected and one where injection was never attempted look
// identical on the row.
//
// The ordering follows DURABILITY, not strength: injected wins even when layer
// 1 was also requested, because layer 2 is the one that survives a lost
// dispatch response. The correlation_ready report in the setup guide
// (docs/setup.md) reads exactly that distinction.
type Correlation string

const (
	// CorrelationInjected — layer 2 is armed: standby_trigger_id was injected.
	// Layer 1 was requested too unless it is switched off.
	CorrelationInjected Correlation = "injected"
	// CorrelationDispatchResponse — layer 1 only: return_run_details was
	// requested but injection was not possible (the workflow does not declare
	// standby_trigger_id, or its definition is unknown).
	CorrelationDispatchResponse Correlation = "dispatch_response"
	// CorrelationWindow — neither: layer 1 is off by configuration
	// (github.return_run_details: false) AND injection was not possible.
	// Unreachable in a default deployment. It stays in the enum because
	// return_run_details may be absent on GHES, and the value sits inside the
	// hash scope on rows that are never updated, so it could not be added later
	// without a schema change.
	CorrelationWindow Correlation = "window"
)

func (c Correlation) valid() bool {
	switch c {
	case CorrelationInjected, CorrelationDispatchResponse, CorrelationWindow:
		return true
	}
	return false
}

// ResolutionSource records which correlation layer actually established a
// run_link. Resolution says how RELIABLE the link is; ResolutionSource says what
// PRODUCED it. Layers 1 and 2 both produce `exact`, so the distinction cannot
// be squeezed into Resolution.
type ResolutionSource string

const (
	// SourceDispatchResponse — layer 1, written on the request path right
	// after the dispatch, in the same transaction as the completion.
	SourceDispatchResponse ResolutionSource = "dispatch_response"
	// SourceRunName — layer 2, the background resolver matching display_title.
	SourceRunName ResolutionSource = "run_name"
	// SourceWindow — layer 3, the background resolver's time window.
	SourceWindow ResolutionSource = "window"
)

func (s ResolutionSource) valid() bool {
	switch s {
	case SourceDispatchResponse, SourceRunName, SourceWindow:
		return true
	}
	return false
}

// RejectReason is the closed enum of trigger_rejected: why an authenticated request was
// refused before reservation.
type RejectReason string

const (
	RejectRateLimited          RejectReason = "rate_limited"
	RejectCallerPolicy         RejectReason = "caller_policy"
	RejectNotInAllowlist       RejectReason = "not_in_allowlist"
	RejectDefaultBranchUnknown RejectReason = "default_branch_unknown"
	RejectBadRequest           RejectReason = "bad_request"
	RejectUnsupportedInputs    RejectReason = "unsupported_inputs"
	RejectSecretDetected       RejectReason = "secret_detected"
)

func (r RejectReason) valid() bool {
	switch r {
	case RejectRateLimited, RejectCallerPolicy, RejectNotInAllowlist,
		RejectDefaultBranchUnknown, RejectBadRequest, RejectUnsupportedInputs,
		RejectSecretDetected:
		return true
	}
	return false
}

// Resolution is the run_id match reliability class. Ordered:
// none < ambiguous < inferred < exact.
type Resolution string

const (
	ResolutionNone      Resolution = "none"
	ResolutionAmbiguous Resolution = "ambiguous"
	ResolutionInferred  Resolution = "inferred"
	ResolutionExact     Resolution = "exact"
)

// rank orders resolutions by reliability. Unknown values rank below
// everything so an unrecognised value can never supersede a real one.
func (r Resolution) rank() int {
	switch r {
	case ResolutionExact:
		return 4
	case ResolutionInferred:
		return 3
	case ResolutionAmbiguous:
		return 2
	case ResolutionNone:
		return 1
	default:
		return 0
	}
}

func (r Resolution) valid() bool { return r.rank() > 0 }

// IdempotencyOutcome is what the trigger event records about the reservation
// Phase 0 writes only "new": replay and conflict do not produce a
// trigger event, see the note on Reserve.
type IdempotencyOutcome string

const (
	IdempotencyNew      IdempotencyOutcome = "new"
	IdempotencyReplay   IdempotencyOutcome = "replay"
	IdempotencyConflict IdempotencyOutcome = "conflict"
)

// TriggerEvent is the `trigger` event.
//
// It carries no outcome fields on purpose. The trigger is written with the
// reservation, with outcome_status = NULL, and the result goes in a separate
// trigger_outcome event; making the outcome unrepresentable here is what stops
// the append-only rule from being broken by accident.
type TriggerEvent struct {
	EventID string
	TS      time.Time

	// OriginForge defaults to "github" when empty; Epoch is 0 in Phase 0.
	OriginForge string
	Epoch       int64

	CallerTokenID string
	CallerName    string
	// CallerSourceIP and CallerUserAgent are redactable: `standpipe forget`
	// NULLs them and content_sha256 stays valid because they are outside the
	// hash scope.
	CallerSourceIP  string
	CallerUserAgent string

	TargetProject  string
	TargetWorkflow string
	TargetRef      string

	InputsHash       string
	InputsValidation InputsValidation

	// Correlation is REQUIRED on every trigger event (a table-level CHECK
	// enforces it). There is no default: the value is a claim about what the gateway
	// armed this dispatch with, it is inside the hash scope, and the row is
	// never updated, so guessing it here would forge the log.
	Correlation Correlation

	IdempotencyKey     string
	IdempotencyOutcome IdempotencyOutcome

	// RequireRaw is the raw request-body `require` value, or nil.
	RequireRaw *string

	DecisionBackend string
	DecisionReason  string

	// ReasonText is redactable, max 1024 characters.
	ReasonText string

	RequestID string
}

// TriggerOutcomeEvent is the `trigger_outcome` event.
type TriggerOutcomeEvent struct {
	EventID        string
	TS             time.Time
	TriggerEventID string

	// OriginForge and Epoch are the archive envelope, and on this event
	// type they are HASHED FROM THE ROW rather than assumed. They
	// were the one place where the scope fixed them at "github"/0 because the
	// writer did; that left both columns outside this type's integrity scope
	// entirely, so an edit to either would have gone unnoticed.
	//
	// Empty OriginForge still means "github" (see originForge), and Epoch 0 is
	// Phase 0's only value, so a caller that leaves both zero produces exactly
	// the canonical bytes the constants produced. That is what makes the
	// widening free today — and it stops being free the moment a row carries
	// anything else, because a trigger_outcome row is never updated and could
	// not be re-hashed.
	OriginForge string
	Epoch       int64

	OutcomeStatus     OutcomeStatus
	OutcomeReason     string
	OutcomeHTTPStatus int
	UpstreamDuration  time.Duration
}

// RunLinkEvent is the `run_link` event.
type RunLinkEvent struct {
	EventID        string // optional; minted when empty
	TS             time.Time
	TriggerEventID string
	// RunID is nil when Resolution is "none" — the resolver records that it
	// found nothing rather than leaving a bare NULL.
	RunID      *int64
	Resolution Resolution
	// ResolutionSource is REQUIRED (a table-level CHECK enforces it): which
	// correlation layer produced this link.
	ResolutionSource ResolutionSource

	OriginForge string
	Epoch       int64
}

// TriggerRejectedEvent is the parentless `trigger_rejected` event.
//
// It carries no idempotency key, no reservation and no parent: it records a
// request that was authenticated and then refused before the reservation step,
// when there was not yet any row for an outcome to hang on. It is deliberately NOT a
// trigger + trigger_outcome pair — such a pair would enter the resolver queue
// and the coverage join as evidence of a dispatch that never
// happened.
type TriggerRejectedEvent struct {
	EventID string // optional; minted when empty
	TS      time.Time

	OriginForge string
	Epoch       int64

	CallerTokenID string
	CallerName    string
	// CallerSourceIP and CallerUserAgent are redactable: Forget NULLs
	// them on trigger_rejected rows too, otherwise the IPs of rejected callers
	// are left behind — a silent compliance gap.
	CallerSourceIP  string
	CallerUserAgent string

	TargetProject  string
	TargetWorkflow string
	// TargetRef may be empty on a rejection that happened before body
	// validation.
	TargetRef string

	RejectReason RejectReason
	// HTTPStatus is 400, 403, 422 or 429.
	HTTPStatus int

	// SuppressedCount and SuppressedSince are set ONLY for rate_limited
	// (429 coalescing): one row per caller per reject_log_window,
	// carrying how many requests it stands for and when the window opened.
	// SuppressedCount must be >= 1 and SuppressedSince non-zero when set.
	SuppressedCount int
	SuppressedSince time.Time

	// ReasonText is redactable.
	ReasonText string

	RequestID string
}

// RunObservedEvent is the `run_observed` event.
type RunObservedEvent struct {
	EventID        string // optional; minted when empty
	TS             time.Time
	TriggerEventID string
	RunID          int64
	Conclusion     string

	OriginForge string
	Epoch       int64
}

// IdempotencyRecord is a row of the idempotency table.
type IdempotencyRecord struct {
	CallerName       string
	IdempotencyKey   string
	CallerTokenID    string
	RequestPath      string
	BodySHA256       []byte
	State            State
	ResponseStatus   int
	ResponseBody     string
	EventID          string
	AttemptStartedAt time.Time
	// DispatchStartedAt belongs to the attempt that currently owns the row and
	// is the crash-recovery sweep's liveness discriminator: non-NULL means the request
	// is inside a GitHub call. The reclaim CAS clears it, because the new
	// attempt has not dispatched yet.
	DispatchStartedAt *time.Time
	// PriorDispatchStartedAt is the most recent moment at which any SUPERSEDED
	// attempt on this key began a dispatch. Nothing reads it as liveness; it
	// exists so that clearing DispatchStartedAt on reclaim does not destroy
	// the evidence that a POST already went out, which the replay path needs
	// before it may dispatch again. The CAS carries it
	// forward with COALESCE, so it survives a chain of reclaims.
	PriorDispatchStartedAt *time.Time
	CreatedAt              time.Time
	ExpiresAt              time.Time
}

// WorkflowCacheEntry is a row of workflow_cache. The cache is
// persistent and survives restart.
type WorkflowCacheEntry struct {
	Repo     string
	Workflow string
	// Ref is the ref the definition was fetched from; "default" in Phase 0.
	Ref        string
	WorkflowID int64
	Triggers   []string // stored as a JSON array
	// InputsDef is the raw JSON of on.workflow_dispatch.inputs.
	//
	// The empty string and "{}" are NOT the same value and must not be
	// collapsed. "" is stored as SQL NULL and means the definition is
	// UNKNOWN — the workflow file could not be read, so unsupported-input validation is
	// skipped and the event is marked 'skipped_cold_cache'. "{}" is stored as
	// '{}' and means the file was read and declares no inputs, so any input
	// key the caller sends is a 422 unsupported_inputs.
	InputsDef     string
	DefaultBranch string
	FetchedAt     time.Time
}

// CorrelationReady reports whether standby_trigger_id is defined in the cached
// inputs definition (the correlation_ready field of `standpipe inventory`; see
// docs/setup.md). It is what makes conditional injection
// observable; a false here is the answer to "why is the unknown rate high".
func (e WorkflowCacheEntry) CorrelationReady() bool { return e.hasInput(reservedInputKey) }

// GitHubRun is a row of github_runs. This table is a cache, not part of
// the append-only log.
type GitHubRun struct {
	RunID                int64
	Repo                 string
	WorkflowID           int64
	Workflow             string
	Event                string
	HeadBranch           string
	HeadBranchNormalized string
	Actor                string
	TriggeringActor      string
	Conclusion           string
	CreatedAt            time.Time
	FetchedAt            time.Time
}

// ConclusionChange reports a run whose conclusion changed on the last upsert
// and that has an active run_link, i.e. the trigger condition for a
// run_observed event.
type ConclusionChange struct {
	RunID          int64
	TriggerEventID string
	From           string
	To             string
}
