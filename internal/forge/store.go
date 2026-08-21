package forge

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// Consumer-side storage interfaces.
//
// package forge does not import internal/store and assumes nothing about its
// signatures. These interfaces state what this package needs and
// internal/store implements them. The types below mirror the gateway.db DDL
// but are this package's own — no SQL is written here.
// ---------------------------------------------------------------------------

// InputDef is one entry of a workflow's on.workflow_dispatch.inputs block.
// Serialized into workflow_cache.inputs_def.
type InputDef struct {
	Description string   `json:"description,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Type        string   `json:"type,omitempty"`
	Default     string   `json:"default,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// WorkflowCacheEntry is one row of workflow_cache. The key is the
// (repo, workflow, ref) triple: workflow_dispatch runs the file as it exists
// on the given ref, so different branches can define different inputs.
type WorkflowCacheEntry struct {
	Repo       string
	Workflow   string // file name, e.g. "release.yml"
	Ref        string // which ref it was fetched from; "default" in Phase 0
	WorkflowID int64
	// Triggers is the set of event names in the file's on: block.
	Triggers []string
	// InputsDef is nil when the definition is NOT KNOWN (inputs_def NULL:
	// the workflow file could not be read), and non-nil-but-empty when the
	// file was read and declares no inputs. The distinction decides between
	// skipping validation and rejecting an unsupported key, so it must
	// survive the round trip through SQLite as NULL vs '{}'.
	InputsDef     map[string]InputDef
	DefaultBranch string
	FetchedAt     time.Time
}

// Stale reports whether the entry is past its TTL. A stale entry is
// still usable and is refreshed in the background. Only a never-fetched
// workflow is cold, and cold is a degradation mode that gets a counter.
func (e WorkflowCacheEntry) Stale(ttl time.Duration, now time.Time) bool {
	return now.Sub(e.FetchedAt) > ttl
}

// HasTrigger reports whether the workflow declares an event.
func (e WorkflowCacheEntry) HasTrigger(event string) bool {
	for _, t := range e.Triggers {
		if t == event {
			return true
		}
	}
	return false
}

// RunRecord is one row of github_runs. That table is a cache, not an
// event log: conclusion changes over time and the row is upserted.
type RunRecord struct {
	RunID                int64
	Repo                 string
	WorkflowID           int64
	Workflow             string // file name, derived from the run's path
	Event                string
	HeadBranch           string
	HeadBranchNormalized string
	Actor                string
	TriggeringActor      string
	Conclusion           string
	CreatedAt            time.Time
	FetchedAt            time.Time
}

// Resolution is the correlation reliability class of a run_link. It is always
// written — a bare NULL would hide how the link was established.
type Resolution string

const (
	// ResolutionExact is a standby_trigger_id match in the run-name.
	ResolutionExact Resolution = "exact"
	// ResolutionInferred is a single candidate in the time window with a
	// matching ref.
	ResolutionInferred Resolution = "inferred"
	// ResolutionAmbiguous is more than one candidate; the nearest was taken.
	ResolutionAmbiguous Resolution = "ambiguous"
	// ResolutionNone is no candidate before the horizon elapsed.
	ResolutionNone Resolution = "none"
)

// rank orders resolutions by reliability: none < ambiguous < inferred
// < exact. A weaker resolution never overwrites a stronger one.
func (r Resolution) rank() int {
	switch r {
	case ResolutionExact:
		return 3
	case ResolutionInferred:
		return 2
	case ResolutionAmbiguous:
		return 1
	default:
		return 0
	}
}

// StrongerThan reports whether r should supersede other.
func (r Resolution) StrongerThan(other Resolution) bool { return r.rank() > other.rank() }

// PendingTrigger is one forwarded trigger event with no active run_link yet —
// the result of the correlation resolver query. The query lives in internal/store;
// this is only its shape.
type PendingTrigger struct {
	EventID  string
	Repo     string
	Workflow string
	Ref      string
	At       time.Time
	// Correlation is what the dispatch recorded (the armed tier). Only
	// CorrelationInjected arms tier 2, i.e. means the run name should carry
	// EventID; test it through Correlation.ArmsRunNameMatch and not by
	// comparing against the other two values.
	//
	// The adapter MUST map this from the stored trigger row. A zero value here
	// is not "unknown" — it silently reads as "tier 2 not armed" and sends every
	// trigger down the time-window path, which downgrades exact links to
	// inferred/ambiguous with no error anywhere.
	Correlation Correlation
	// ExistingResolution is the resolution of the superseded run_link, if the
	// trigger already has a weak one. Empty when there is none.
	ExistingResolution Resolution
}

// RunObserved is a run_observed event: a run's conclusion, recorded once
// it is known. The resolver writes one immediately after a run_link
// when the matched run already carries a conclusion; otherwise the inventory
// refresh produces it later.
type RunObserved struct {
	TriggerEventID string
	RunID          int64
	Conclusion     string
	At             time.Time
}

// ResolutionSource is the run_link record of WHICH TIER produced a run_link.
//
// It is the post-dispatch half of a fact whose pre-dispatch half is
// Correlation: `correlation` says what was armed, `resolution_source` says what
// fired. The two cannot be one field — correlation is inside the trigger row's
// hash scope and that row is never updated, while the tier is only known after
// the response.
//
// It exists at all because tiers 1 and 2 BOTH produce ResolutionExact: without
// it the log cannot answer "did the operator's run-name edit do anything", and
// the operator checklist's adoption report and `standpipe trace`'s tier column have no source.
type ResolutionSource string

const (
	// ResolutionSourceDispatchResponse is tier 1: the run id came back in the
	// dispatch response. Written on the request path, right after the dispatch, not by the
	// resolver.
	ResolutionSourceDispatchResponse ResolutionSource = "dispatch_response"
	// ResolutionSourceRunName is tier 2: standby_trigger_id was found in the
	// run name. Written by the background resolver.
	ResolutionSourceRunName ResolutionSource = "run_name"
	// ResolutionSourceWindow is tier 3: the time window plus ref. Every link
	// the tier-3 path writes carries this, including ResolutionNone.
	ResolutionSourceWindow ResolutionSource = "window"
)

// RunLink is a run_link event. RunID is zero when Resolution is
// ResolutionNone — the resolver records explicitly that it found nothing.
//
// ResolutionSource is MANDATORY on every run_link (the DDL has a CHECK for it);
// the zero value is not a legal column value, so every construction site must
// set it.
type RunLink struct {
	TriggerEventID   string
	RunID            int64
	Resolution       Resolution
	ResolutionSource ResolutionSource
	At               time.Time
}

// RunLinkFromDispatch builds the tier-1 run_link for a dispatch whose response
// carried a run id (correlation tier 1). ok is false when there is no
// run id to link — tier 1 was off, the body was empty, or the response was
// lost — in which case NOTHING is written and the background resolver picks the
// trigger up as usual.
//
// It lives here so the composition root never has to spell the enum literals
// itself: a hand-written "dispatch_response" that drifts would misattribute the
// tier with no symptom other than a wrong adoption report.
func RunLinkFromDispatch(triggerEventID string, res *DispatchResult, at time.Time) (RunLink, bool) {
	if res == nil || res.RunID == 0 || triggerEventID == "" {
		return RunLink{}, false
	}
	return RunLink{
		TriggerEventID:   triggerEventID,
		RunID:            res.RunID,
		Resolution:       ResolutionExact,
		ResolutionSource: ResolutionSourceDispatchResponse,
		At:               at,
	}, true
}

// WorkflowCacheStore is the persistent workflow_cache.
type WorkflowCacheStore interface {
	// GetWorkflowCache returns the entry, or ok=false when the workflow has
	// never been fetched (cold).
	GetWorkflowCache(ctx context.Context, repo, workflow, ref string) (entry WorkflowCacheEntry, ok bool, err error)
	// PutWorkflowCache upserts on (repo, workflow, ref).
	PutWorkflowCache(ctx context.Context, entry WorkflowCacheEntry) error
}

// RunHistoryStore is the github_runs bootstrap cache.
type RunHistoryStore interface {
	// UpsertRuns applies the ON CONFLICT(run_id) DO UPDATE semantics of the
	// bootstrap table and returns the runs whose conclusion changed, so the
	// run_observed
	// producer can act on them without re-reading.
	UpsertRuns(ctx context.Context, runs []RunRecord) (changed []RunRecord, err error)
}

// CorrelationStore backs the correlation resolver. The resolver is query-based, not
// timer-based: its whole state is these two calls, so a restart resumes
// without losing a pending correlation.
type CorrelationStore interface {
	// PendingCorrelations returns forwarded triggers with no active run_link,
	// newer than horizon. Ordering is oldest first.
	PendingCorrelations(ctx context.Context, horizon time.Duration, limit int) ([]PendingTrigger, error)
	// WriteRunLink appends a run_link event. It must apply the run_link
	// supersession rule: a link weaker than the active one is not written,
	// a stronger one sets superseded_by on the old row. Supersession compares
	// Resolution only — ResolutionSource is recorded, never ranked.
	WriteRunLink(ctx context.Context, link RunLink) error
	// WriteRunObserved appends a run_observed event. It is a
	// no-op when an equivalent observation is already recorded.
	WriteRunObserved(ctx context.Context, observed RunObserved) error
}

// Store is everything package forge needs from durable storage.
type Store interface {
	WorkflowCacheStore
	RunHistoryStore
	CorrelationStore
}
