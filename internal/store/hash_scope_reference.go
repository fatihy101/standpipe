package store

// This file declares, per event type, the closed list of fields the
// content_sha256 of a trigger-log row is computed over. internal/docgen
// renders it into docs/reference/hash-scopes.md, and
// hash_scope_reference_test.go fails if a list disagrees with the scope the
// writer actually builds, in either direction.
//
// The lists are deliberately a second enumeration next to the scope builders
// in events.go: a field cannot enter or leave a hash scope without someone
// also editing the list here, and the regenerated reference makes the change
// visible in review.

// ScopeExclusion is a column a row carries that is deliberately outside its
// hash scope, with the reason.
type ScopeExclusion struct {
	Field  string
	Reason string
}

// HashScope is one event type's closed hash-scope list.
type HashScope struct {
	// EventType is the trigger-log event_type value.
	EventType string
	// Fields are the dotted keys inside the scope, in canonical order.
	// A nested key such as caller.token_id is a member of a JSON object in the
	// canonical form.
	Fields []string
	// Excluded are the columns kept OUT of the scope on purpose.
	Excluded []ScopeExclusion
	// Redactable is true for the row types `standpipe forget` touches. Their
	// scopes must not contain any RedactableFields entry, or a forgotten row
	// would stop verifying.
	Redactable bool
	// Note is an optional sentence for the reference.
	Note string
}

// RedactableFields are the fields `standpipe forget` sets to NULL. They are
// outside every hash scope of a redactable row type, which is what lets a row
// be redacted without invalidating its content_sha256.
func RedactableFields() []string {
	return []string{"caller.source_ip", "caller.user_agent", "reason_text"}
}

const (
	selfExclusionReason       = "the hash itself; a self-referential hash cannot be recomputed"
	redactableExclusionReason = "redactable: `standpipe forget` sets it to NULL, so it must stay outside the hash"
)

func redactableExclusions() []ScopeExclusion {
	out := []ScopeExclusion{{Field: "content_sha256", Reason: selfExclusionReason}}
	for _, f := range RedactableFields() {
		out = append(out, ScopeExclusion{Field: f, Reason: redactableExclusionReason})
	}
	return out
}

// HashScopes returns every event type's closed hash-scope list.
func HashScopes() []HashScope {
	return []HashScope{
		{
			EventType: string(EventTrigger),
			Fields: []string{
				"schema_version", "event_type", "event_id", "ts", "origin_forge", "epoch",
				"caller.token_id", "caller.name",
				"target.project", "target.workflow", "target.ref",
				"inputs_hash", "inputs_validation", "correlation",
				"idempotency_key", "idempotency_outcome", "require_raw",
				"decision.backend", "decision.reason",
				"outcome", "request_id",
			},
			Excluded:   redactableExclusions(),
			Redactable: true,
			Note: "`outcome` is hashed as a constant `null`: a trigger row is written with no outcome and " +
				"never updated, because the outcome is carried by its own `trigger_outcome` event.",
		},
		{
			EventType: string(EventRunLink),
			Fields: []string{
				"schema_version", "event_type", "event_id", "ts",
				"trigger_event_id", "run_id", "resolution", "resolution_source",
				"origin_forge", "epoch",
			},
			Excluded: []ScopeExclusion{
				{Field: "content_sha256", Reason: selfExclusionReason},
				{Field: "superseded_by", Reason: "the one column a run_link row is ever updated in (when a stronger " +
					"resolution supersedes it); hashing it would make every superseded link fail verification " +
					"without any tampering"},
			},
			Note: "This event type has no redactable field.",
		},
		{
			EventType: string(EventTriggerOutcome),
			Fields: []string{
				"schema_version", "event_type", "event_id", "ts", "origin_forge", "epoch",
				"trigger_event_id", "outcome_status", "outcome_reason",
				"outcome_http_status", "outcome_upstream_duration_ms",
			},
			Excluded: []ScopeExclusion{{Field: "content_sha256", Reason: selfExclusionReason}},
			Note: "No redactable field; the row is final when written. `origin_forge` and `epoch` are hashed " +
				"from the row's own values rather than assumed constant, so changing either in an archive is " +
				"detected.",
		},
		{
			EventType: string(EventTriggerRejected),
			Fields: []string{
				"schema_version", "event_type", "event_id", "ts", "origin_forge", "epoch",
				"caller.token_id", "caller.name",
				"target.project", "target.workflow", "target.ref",
				"reject_reason", "outcome_http_status",
				"suppressed_count", "suppressed_since", "request_id",
			},
			Excluded:   redactableExclusions(),
			Redactable: true,
			Note: "`suppressed_*` are inside the scope because the row is final when written: the counter " +
				"accumulates in memory and the row is written once, when the coalescing window closes.",
		},
		{
			EventType: string(EventRunObserved),
			Fields: []string{
				"schema_version", "event_type", "event_id", "ts",
				"trigger_event_id", "run_id", "conclusion", "origin_forge", "epoch",
			},
			Excluded: []ScopeExclusion{{Field: "content_sha256", Reason: selfExclusionReason}},
			Note: "No redactable field and nothing is ever updated, so the scope is every field of the event " +
				"except `content_sha256`. There is no `status` field: only GitHub's `conclusion` is recorded.",
		},
	}
}
