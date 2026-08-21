package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// IntegrityVerdict is the answer VerifyContentHash reaches about one event.
//
// It is THREE-valued on purpose. A boolean forces "I could not check this"
// into one of the two confident answers, and both directions are wrong: false
// reads as tampering on a perfectly healthy row, true reads as a verification
// that never ran. `standpipe trace` prints this verdict as the operator's evidence
// that the log is intact, so a verdict that overstates what was checked is
// worse than no verdict at all: a quiet "success" is the gravest failure mode.
type IntegrityVerdict string

const (
	// IntegrityOK — the hash scope was rebuilt from the stored row and the
	// recomputed content_sha256 equals the stored one.
	IntegrityOK IntegrityVerdict = "ok"
	// IntegrityMismatch — the scope was rebuilt and the values differ. The
	// row's recorded content is not the content its hash was taken over.
	IntegrityMismatch IntegrityVerdict = "mismatch"
	// IntegrityUnverifiable — no comparison was made. IntegrityResult.Reason
	// says why. This is NOT a weaker mismatch and must never be rendered as
	// one.
	IntegrityUnverifiable IntegrityVerdict = "unverifiable"
)

// Verified is the only predicate that authorises the claim "this event's
// content_sha256 checks out". It is false for the zero value by construction,
// so an IntegrityResult that was never filled in cannot be read as a pass —
// the same shape as ReserveOutcome.MayDispatch.
func (v IntegrityVerdict) Verified() bool { return v == IntegrityOK }

// IntegrityReason says why a verdict is IntegrityUnverifiable. It is empty on
// IntegrityOK and IntegrityMismatch.
type IntegrityReason string

const (
	// IntegrityReasonRowMissing — no event with that id. Retention may
	// have purged it; that is not a failure and not a corruption.
	IntegrityReasonRowMissing IntegrityReason = "row_missing"

	// IntegrityReasonUnknownEventType — the row's event_type is not one this
	// build knows. The hash scope is a CLOSED per-type field list, so there
	// is no scope to rebuild. This is what a database written by a newer build
	// looks like, and it is the same refusal ExportEvents makes.
	IntegrityReasonUnknownEventType IntegrityReason = "unknown_event_type"

	// IntegrityReasonUnknownSchemaVersion — the row's schema_version is not
	// this build's. schema_version is INSIDE the hash scope, so
	// recomputing a version-N row with the version-M field list produces a
	// confident MISMATCH on a healthy row. An archive may carry
	// rows of several versions side by side, so this is a legitimate row this
	// binary simply cannot speak for.
	IntegrityReasonUnknownSchemaVersion IntegrityReason = "unknown_schema_version"

	// IntegrityReasonRowMalformed — the row does not satisfy an invariant its
	// own event type's writer and CHECK constraints guarantee (a NULL where
	// the type requires a value, or a value where the hash scope hardcodes a
	// constant). The scope cannot describe such a row, so recomputing over it
	// would answer a question about a different row than the one stored.
	// IntegrityResult.Detail names the field.
	IntegrityReasonRowMalformed IntegrityReason = "row_malformed"

	// IntegrityReasonStoredHashMissing — content_sha256 is empty. NOT NULL in
	// the schema, so this only occurs on a row that did not come through this
	// package; there is nothing to compare against.
	IntegrityReasonStoredHashMissing IntegrityReason = "stored_hash_missing"
)

// IntegrityResult is VerifyContentHash's full answer.
//
// Every field except Verdict exists so the verdict is explicable afterwards.
// The zero value is IntegrityUnverifiable with no reason, which is the safe
// reading of "nobody filled this in".
type IntegrityResult struct {
	// EventID is the id that was asked about, echoed back.
	EventID string
	// EventType is the row's event_type, empty when the row is missing. It is
	// echoed even when unknown, because "which type could not be verified" is
	// the operator's next question.
	EventType EventType

	Verdict IntegrityVerdict
	// Reason is set only when Verdict is IntegrityUnverifiable.
	Reason IntegrityReason

	// Stored is content_sha256 as it stands in the row, empty when the row is
	// missing.
	Stored string
	// Computed is the value recomputed from the stored row over the hash scope.
	// It is empty when Verdict is IntegrityUnverifiable — nothing was
	// computed, and printing a value there would suggest a comparison happened.
	Computed string

	// Detail is a human-readable sentence for the unverifiable and mismatch
	// cases, suitable for `standpipe trace` output. Empty on IntegrityOK.
	Detail string
}

// VerifyContentHash recomputes one event's content_sha256 from the stored row
// and compares it with the value the row carries (`standpipe trace` reports it).
//
// # It recomputes, it does not re-read
//
// The scope is rebuilt by calling the SAME per-type builders the writer used —
// triggerHashScope, triggerOutcomeHashScope, triggerRejectedHashScope,
// runLinkHashScope, runObservedHashScope — over an event struct reconstructed
// from the row's columns. The stored content_sha256 is read once, at the end,
// only to be compared. Nothing here re-derives the answer from the answer.
//
// This is also why the entry point lives in this package rather than in
// internal/cli. The five builders are unexported and the scope is a closed
// per-type field list with deliberate exclusions — the redactable fields,
// content_sha256 itself, the always-NULL outcome on a trigger row. A
// reimplementation elsewhere would compile, pass vet, and drift from the scope the
// first time a field moved, producing a confident MISMATCH on a healthy row.
//
// # A redacted row still verifies
//
// `standpipe forget` NULLs caller_source_ip, caller_user_agent and
// reason_text. None of the three is in any hash scope, and the reconstruction
// below deliberately leaves them at their zero values rather than reading the
// columns — so a forgotten row rebuilds to exactly the scope its hash was
// taken over and verifies clean. That property is the whole reason the
// exclusions exist, and it is checked by an existing test.
//
// # error versus verdict
//
// A missing row is a VERDICT (IntegrityUnverifiable / row_missing), not
// ErrNotFound. All three cannot-verify causes come back through the same
// field, so a caller cannot handle one and let the others fall through into a
// confident claim. err is reserved for a failure to ask the question at all:
// a query error, or a scope that will not canonicalise.
//
// It runs on the read pool and takes no lock, so it is safe while a dispatch
// is in flight.
func (db *DB) VerifyContentHash(ctx context.Context, eventID string) (IntegrityResult, error) {
	out := IntegrityResult{EventID: eventID, Verdict: IntegrityUnverifiable}
	if eventID == "" {
		return out, errors.New("store: VerifyContentHash needs an event_id")
	}

	var r eventRow
	err := r.scan(db.reader.QueryRowContext(ctx, selectEventsSQL+` WHERE event_id = ?`, eventID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		out.Reason = IntegrityReasonRowMissing
		out.Detail = fmt.Sprintf("no event with id %s; the retention window may have purged it", eventID)
		return out, nil
	case err != nil:
		return out, err
	}

	out.EventType = EventType(r.eventType)
	out.Stored = r.contentSHA

	if r.contentSHA == "" {
		out.Reason = IntegrityReasonStoredHashMissing
		out.Detail = "the row carries no content_sha256, so there is nothing to compare a recomputation with"
		return out, nil
	}
	if r.schemaVersion != schemaVersionOfEvents {
		out.Reason = IntegrityReasonUnknownSchemaVersion
		out.Detail = fmt.Sprintf("row schema_version is %d and this build writes %d; "+
			"schema_version is inside the hash scope, so recomputing with this build's field list "+
			"would report a mismatch on a row that is not damaged",
			r.schemaVersion, schemaVersionOfEvents)
		return out, nil
	}

	scope, reason, detail := r.rebuildHashScope()
	if reason != "" {
		out.Reason, out.Detail = reason, detail
		return out, nil
	}

	computed, err := contentSHA256(scope)
	if err != nil {
		return out, fmt.Errorf("store: recomputing content_sha256 for %s: %w", eventID, err)
	}
	out.Computed = computed
	if computed == r.contentSHA {
		out.Verdict = IntegrityOK
		return out, nil
	}
	out.Verdict = IntegrityMismatch
	out.Detail = fmt.Sprintf("%s event %s: the row records %s but its content hashes to %s",
		r.eventType, eventID, r.contentSHA, computed)
	return out, nil
}

// rebuildHashScope reconstructs the event struct the writer held and returns
// the scope its own builder produces from it. A non-empty IntegrityReason
// means no scope could be built, and the second string explains which
// invariant the row failed.
//
// The reconstruction has to be EXACT, including the empty-string-versus-NULL
// convention: every nullable TEXT column in the events table stores "" as NULL (see
// nullable), so a NULL column reads back as "" and passes through the same
// nullable() call in the builder. That round trip is what makes recomputation
// meaningful rather than approximate.
func (r *eventRow) rebuildHashScope() (map[string]any, IntegrityReason, string) {
	malformed := func(format string, args ...any) (map[string]any, IntegrityReason, string) {
		return nil, IntegrityReasonRowMalformed, fmt.Sprintf(format, args...)
	}
	ts := time.UnixMilli(r.ts)

	switch EventType(r.eventType) {
	case EventTrigger:
		// correlation is NOT NULL on a trigger row (a table-level CHECK
		// enforces it) and is inside the scope. A NULL here would silently rebuild
		// as an absent value and report a mismatch against a row whose real
		// problem is that it was not written by this schema.
		if !r.correlation.Valid {
			return malformed("trigger row %s has a NULL correlation, which the table's CHECK forbids", r.eventID)
		}
		// The trigger is written with a NULL outcome and the gateway puts
		// the result in its own event, which is why the scope's "outcome" is a
		// hardcoded null. A trigger row that carries an outcome is outside
		// what the scope can describe: recomputing would hash a null and
		// report OK while the row says something else.
		//
		// The table's CHECK now forbids that row at write time, so this branch is
		// unreachable through any INSERT this database accepts. It STAYS, and
		// deliberately, because `row_malformed` remains a live verdict
		// for exactly this shape, which has two remaining sources: a row
		// written before the CHECK existed, or a database edited from outside
		// this package. A CHECK constrains writes through SQLite's own
		// enforcement; it does not constrain the bytes a verifier is handed —
		// `PRAGMA writable_schema`, a hand-edited file, a restore of a
		// pre-migration snapshot. Dropping the branch would turn each of those
		// into a confident `ok`, which is the one answer this function exists
		// to never give. It costs four nil checks.
		//
		// All FOUR columns, matching the CHECK: the trigger scope is one constant
		// null standing for the whole `outcome` object, so a row carrying only
		// outcome_http_status is just as undescribable as one carrying a
		// status.
		switch {
		case r.outcomeStatus.Valid:
			return malformed("trigger row %s carries outcome_status=%q; the trigger hash scope hashes a trigger's outcome as null "+
				"(the result goes in a separate trigger_outcome event), so this row's hash "+
				"cannot speak for its own content", r.eventID, r.outcomeStatus.String)
		case r.outcomeReason.Valid:
			return malformed("trigger row %s carries outcome_reason=%q; the trigger hash scope hashes a trigger's outcome as a "+
				"single null covering all four outcome_* columns", r.eventID, r.outcomeReason.String)
		case r.outcomeHTTPStatus.Valid:
			return malformed("trigger row %s carries outcome_http_status=%d; the trigger hash scope hashes a trigger's outcome as a "+
				"single null covering all four outcome_* columns", r.eventID, r.outcomeHTTPStatus.Int64)
		case r.outcomeDurationMS.Valid:
			return malformed("trigger row %s carries outcome_upstream_duration_ms=%d; the trigger hash scope hashes a trigger's "+
				"outcome as a single null covering all four outcome_* columns", r.eventID, r.outcomeDurationMS.Int64)
		}
		ev := TriggerEvent{
			EventID:        r.eventID,
			TS:             ts,
			OriginForge:    r.originForge,
			Epoch:          r.epoch,
			CallerTokenID:  r.callerTokenID.String,
			CallerName:     r.callerName.String,
			TargetProject:  r.targetProject.String,
			TargetWorkflow: r.targetWorkflow.String,
			TargetRef:      r.targetRef.String,

			InputsHash:       r.inputsHash.String,
			InputsValidation: InputsValidation(r.inputsValidation.String),
			Correlation:      Correlation(r.correlation.String),

			IdempotencyKey:     r.idempotencyKey.String,
			IdempotencyOutcome: IdempotencyOutcome(r.idempotencyOutcome.String),

			DecisionBackend: r.decisionBackend.String,
			DecisionReason:  r.decisionReason.String,
			RequestID:       r.requestID.String,
			// CallerSourceIP, CallerUserAgent and ReasonText are deliberately
			// NOT read. They are the redactable fields and they are
			// outside the scope, so leaving them zero is what makes a
			// row that `standpipe forget` has emptied verify exactly as it did
			// before.
		}
		if r.requireRaw.Valid {
			s := r.requireRaw.String
			ev.RequireRaw = &s
		}
		return triggerHashScope(ev), "", ""

	case EventTriggerOutcome:
		if !r.triggerEventID.Valid {
			return malformed("trigger_outcome row %s has no trigger_event_id", r.eventID)
		}
		if !r.outcomeStatus.Valid {
			return malformed("trigger_outcome row %s has a NULL outcome_status", r.eventID)
		}
		// origin_forge and epoch are read from the row. They used
		// to be a row_malformed case here, because the scope fixed them at
		// "github"/0 and a row that disagreed had content the hash did not
		// cover. Now the scope hashes the columns, so such a row is an
		// ordinary verifiable row and any edit to either column surfaces as a
		// mismatch instead of going unnoticed. The archive envelope wants those columns to
		// carry other values one day; a row that does so is not malformed.
		return triggerOutcomeHashScope(TriggerOutcomeEvent{
			EventID:           r.eventID,
			TS:                ts,
			TriggerEventID:    r.triggerEventID.String,
			OriginForge:       r.originForge,
			Epoch:             r.epoch,
			OutcomeStatus:     OutcomeStatus(r.outcomeStatus.String),
			OutcomeReason:     r.outcomeReason.String,
			OutcomeHTTPStatus: int(r.outcomeHTTPStatus.Int64),
			UpstreamDuration:  time.Duration(r.outcomeDurationMS.Int64) * time.Millisecond,
		}), "", ""

	case EventTriggerRejected:
		if !r.rejectReason.Valid {
			return malformed("trigger_rejected row %s has a NULL reject_reason, which the table's CHECK forbids", r.eventID)
		}
		if !r.outcomeHTTPStatus.Valid {
			return malformed("trigger_rejected row %s has a NULL outcome_http_status, which the table's CHECK forbids", r.eventID)
		}
		ev := TriggerRejectedEvent{
			EventID:        r.eventID,
			TS:             ts,
			OriginForge:    r.originForge,
			Epoch:          r.epoch,
			CallerTokenID:  r.callerTokenID.String,
			CallerName:     r.callerName.String,
			TargetProject:  r.targetProject.String,
			TargetWorkflow: r.targetWorkflow.String,
			TargetRef:      r.targetRef.String,
			RejectReason:   RejectReason(r.rejectReason.String),
			HTTPStatus:     int(r.outcomeHTTPStatus.Int64),
			RequestID:      r.requestID.String,
			// suppressed_* ARE in scope: the row is final the moment
			// it is written. Redactable fields are not read, same as above.
			SuppressedCount: int(r.suppressedCount.Int64),
		}
		if r.suppressedSince.Valid {
			ev.SuppressedSince = time.UnixMilli(r.suppressedSince.Int64)
		}
		return triggerRejectedHashScope(ev), "", ""

	case EventRunLink:
		if !r.triggerEventID.Valid {
			return malformed("run_link row %s has no trigger_event_id", r.eventID)
		}
		if !r.resolution.Valid {
			return malformed("run_link row %s has a NULL resolution", r.eventID)
		}
		if !r.resolutionSource.Valid {
			return malformed("run_link row %s has a NULL resolution_source, which the table's CHECK forbids", r.eventID)
		}
		ev := RunLinkEvent{
			EventID:          r.eventID,
			TS:               ts,
			TriggerEventID:   r.triggerEventID.String,
			Resolution:       Resolution(r.resolution.String),
			ResolutionSource: ResolutionSource(r.resolutionSource.String),
			OriginForge:      r.originForge,
			Epoch:            r.epoch,
		}
		if r.runID.Valid {
			id := r.runID.Int64
			ev.RunID = &id
		}
		// superseded_by is not read and is not in the scope: the store expresses a
		// supersede by stamping that pointer on a row whose content is
		// otherwise final, so it is the one column that legitimately changes
		// after the hash was taken.
		return runLinkHashScope(ev), "", ""

	case EventRunObserved:
		if !r.triggerEventID.Valid {
			return malformed("run_observed row %s has no trigger_event_id", r.eventID)
		}
		if !r.runID.Valid {
			return malformed("run_observed row %s has a NULL run_id", r.eventID)
		}
		if !r.conclusion.Valid {
			return malformed("run_observed row %s has a NULL conclusion; AppendRunObserved refuses to write one", r.eventID)
		}
		return runObservedHashScope(RunObservedEvent{
			EventID:        r.eventID,
			TS:             ts,
			TriggerEventID: r.triggerEventID.String,
			RunID:          r.runID.Int64,
			Conclusion:     r.conclusion.String,
			OriginForge:    r.originForge,
			Epoch:          r.epoch,
		}), "", ""

	default:
		return nil, IntegrityReasonUnknownEventType, fmt.Sprintf(
			"event %s has event_type %q, which this build has no hash scope for; the hash scope is a closed "+
				"per-type field list, so there is nothing to recompute", r.eventID, r.eventType)
	}
}
