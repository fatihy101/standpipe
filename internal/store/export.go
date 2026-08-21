package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// eventColumns is the read side of the polymorphic event table. It is one list
// rather than a hand-written SELECT plus a hand-written Scan argument list: the
// two have to agree positionally, and a column added to only one of them binds
// every following value to the wrong field without any error. The init check
// below turns that into a startup panic instead.
var eventColumns = []string{
	"event_id", "schema_version", "event_type", "ts", "origin_forge", "epoch", "content_sha256",
	"caller_token_id", "caller_name", "caller_source_ip", "caller_user_agent",
	"target_project", "target_workflow", "target_ref",
	"inputs_hash", "inputs_validation", "correlation",
	"idempotency_key", "idempotency_outcome", "require_raw",
	"decision_backend", "decision_reason",
	"outcome_status", "outcome_reason", "outcome_http_status", "outcome_upstream_duration_ms",
	"reject_reason", "suppressed_count", "suppressed_since",
	"reason_text", "request_id",
	"trigger_event_id", "run_id", "resolution", "resolution_source", "superseded_by", "conclusion",
}

var selectEventsSQL = `SELECT ` + strings.Join(eventColumns, ", ") + ` FROM trigger_events`

type eventRow struct {
	eventID       string
	schemaVersion int64
	eventType     string
	ts            int64
	originForge   string
	epoch         int64
	contentSHA    string

	callerTokenID, callerName, callerSourceIP, callerUserAgent sql.NullString
	targetProject, targetWorkflow, targetRef                   sql.NullString
	inputsHash, inputsValidation, correlation                  sql.NullString
	idempotencyKey, idempotencyOutcome, requireRaw             sql.NullString
	decisionBackend, decisionReason                            sql.NullString
	outcomeStatus, outcomeReason                               sql.NullString
	outcomeHTTPStatus, outcomeDurationMS                       sql.NullInt64
	rejectReason                                               sql.NullString
	suppressedCount, suppressedSince                           sql.NullInt64
	reasonText, requestID                                      sql.NullString
	triggerEventID                                             sql.NullString
	runID                                                      sql.NullInt64
	resolution, resolutionSource, supersededBy, conclusion     sql.NullString
}

// dest returns scan targets in exactly eventColumns' order.
func (r *eventRow) dest() []any {
	return []any{
		&r.eventID, &r.schemaVersion, &r.eventType, &r.ts, &r.originForge, &r.epoch, &r.contentSHA,
		&r.callerTokenID, &r.callerName, &r.callerSourceIP, &r.callerUserAgent,
		&r.targetProject, &r.targetWorkflow, &r.targetRef,
		&r.inputsHash, &r.inputsValidation, &r.correlation,
		&r.idempotencyKey, &r.idempotencyOutcome, &r.requireRaw,
		&r.decisionBackend, &r.decisionReason,
		&r.outcomeStatus, &r.outcomeReason, &r.outcomeHTTPStatus, &r.outcomeDurationMS,
		&r.rejectReason, &r.suppressedCount, &r.suppressedSince,
		&r.reasonText, &r.requestID,
		&r.triggerEventID, &r.runID, &r.resolution, &r.resolutionSource, &r.supersededBy, &r.conclusion,
	}
}

func init() {
	if n := len((&eventRow{}).dest()); n != len(eventColumns) {
		panic(fmt.Sprintf("store: eventColumns has %d entries but eventRow.dest has %d — a column was added to one and not the other", len(eventColumns), n))
	}
}

func (r *eventRow) scan(rows rowScanner) error {
	return rows.Scan(r.dest()...)
}

func ns(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func ni(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

// eventJSON rebuilds the full event object from a row. It includes
// the redactable fields and content_sha256, which the hash scope excludes: the
// export is the record, and the verifier recomputes over the closed scope
// to check it.
//
// An unknown event_type is an ERROR, not an envelope-only row. The export
// never writes a row with only the envelope fields: its content_sha256 could not be
// verified against anything, which quietly empties the log's testability claim.
// A row like that can only come from a database written by a newer build, and
// the honest answer is to refuse to export it.
func (r *eventRow) eventJSON() (map[string]any, error) {
	base := map[string]any{
		"schema_version": r.schemaVersion,
		"event_type":     r.eventType,
		"event_id":       r.eventID,
		"ts":             r.ts,
		"origin_forge":   r.originForge,
		"epoch":          r.epoch,
		"content_sha256": r.contentSHA,
	}
	switch EventType(r.eventType) {
	case EventTrigger:
		var outcome any
		if r.outcomeStatus.Valid {
			outcome = map[string]any{
				"status":               r.outcomeStatus.String,
				"reason":               ns(r.outcomeReason),
				"http_status":          ni(r.outcomeHTTPStatus),
				"upstream_duration_ms": ni(r.outcomeDurationMS),
			}
		}
		base["caller"] = map[string]any{
			"token_id":   ns(r.callerTokenID),
			"name":       ns(r.callerName),
			"source_ip":  ns(r.callerSourceIP),
			"user_agent": ns(r.callerUserAgent),
		}
		base["target"] = map[string]any{
			"project":  ns(r.targetProject),
			"workflow": ns(r.targetWorkflow),
			"ref":      ns(r.targetRef),
		}
		base["inputs_hash"] = ns(r.inputsHash)
		base["inputs_validation"] = ns(r.inputsValidation)
		base["correlation"] = ns(r.correlation)
		base["idempotency_key"] = ns(r.idempotencyKey)
		base["idempotency_outcome"] = ns(r.idempotencyOutcome)
		base["require_raw"] = ns(r.requireRaw)
		base["decision"] = map[string]any{
			"backend": ns(r.decisionBackend),
			"reason":  ns(r.decisionReason),
		}
		base["outcome"] = outcome
		base["reason_text"] = ns(r.reasonText)
		base["request_id"] = ns(r.requestID)
	case EventTriggerOutcome:
		base["trigger_event_id"] = ns(r.triggerEventID)
		base["outcome_status"] = ns(r.outcomeStatus)
		base["outcome_reason"] = ns(r.outcomeReason)
		base["outcome_http_status"] = ni(r.outcomeHTTPStatus)
		base["outcome_upstream_duration_ms"] = ni(r.outcomeDurationMS)
	case EventTriggerRejected:
		base["caller"] = map[string]any{
			"token_id":   ns(r.callerTokenID),
			"name":       ns(r.callerName),
			"source_ip":  ns(r.callerSourceIP),
			"user_agent": ns(r.callerUserAgent),
		}
		base["target"] = map[string]any{
			"project":  ns(r.targetProject),
			"workflow": ns(r.targetWorkflow),
			"ref":      ns(r.targetRef),
		}
		base["reject_reason"] = ns(r.rejectReason)
		base["outcome_http_status"] = ni(r.outcomeHTTPStatus)
		base["suppressed_count"] = ni(r.suppressedCount)
		base["suppressed_since"] = ni(r.suppressedSince)
		base["reason_text"] = ns(r.reasonText)
		base["request_id"] = ns(r.requestID)
	case EventRunLink:
		base["trigger_event_id"] = ns(r.triggerEventID)
		base["run_id"] = ni(r.runID)
		base["resolution"] = ns(r.resolution)
		base["resolution_source"] = ns(r.resolutionSource)
		base["superseded_by"] = ns(r.supersededBy)
	case EventRunObserved:
		base["trigger_event_id"] = ns(r.triggerEventID)
		base["run_id"] = ni(r.runID)
		base["conclusion"] = ns(r.conclusion)
	default:
		return nil, fmt.Errorf("store: event %s has unknown event_type %q; "+
			"exporting it with envelope fields only is refused, because its content_sha256 could not be verified",
			r.eventID, r.eventType)
	}
	return base, nil
}

// ExportEvents writes the trigger log as newline-delimited canonical JSON
// (`standpipe export`). One event per line, in time then event id order.
//
// The scope is EVERY event type — trigger, trigger_outcome, trigger_rejected,
// run_link, run_observed. This file is the archive-absorbability claim made
// testable: every line carries content_sha256 and the closed hash scope
// of its type can be recomputed from it.
func (db *DB) ExportEvents(ctx context.Context, since time.Time, w io.Writer) (int, error) {
	q := selectEventsSQL
	args := []any{}
	if !since.IsZero() {
		q += ` WHERE ts >= ?`
		args = append(args, since.UnixMilli())
	}
	q += ` ORDER BY ts, event_id`

	rows, err := db.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		var r eventRow
		if err := r.scan(rows); err != nil {
			return n, err
		}
		obj, err := r.eventJSON()
		if err != nil {
			return n, err
		}
		line, err := CanonicalValue(obj)
		if err != nil {
			return n, fmt.Errorf("store: serialising event %s: %w", r.eventID, err)
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// EventsByTriggerID reads every event hanging off one trigger — its
// trigger_outcome, its run_links (active AND superseded), its run_observed —
// as the same JSON objects EventByID and ExportEvents produce, ordered by ts
// then event_id.
//
// It exists because `standpipe trace` and `standpipe status` otherwise have to
// scan ExportEvents forward from the trigger's own timestamp and discard
// everything whose trigger_event_id does not match. That is correct at any
// size and linear in the log: tracing a trigger from the far end of a 90-day
// retention window reads every event written since. This is an index seek on
// idx_trigger_events_trigger_ref instead.
//
// The trigger row itself is NOT included — it has no trigger_event_id, it is
// the thing these events point at, and the caller already holds it (that is
// where the id came from). An empty slice is the ordinary answer for a trigger
// that has not been completed or resolved yet, not an error; ErrNotFound is
// deliberately not returned, because "this trigger has no children" and "this
// trigger does not exist" are different questions and only EventByID answers
// the second one.
//
// Superseded run_links are returned alongside the active one (append-only
// supersession): filtering them out here would make `trace
// --history` impossible without reintroducing the scan.
func (db *DB) EventsByTriggerID(ctx context.Context, triggerEventID string) ([]map[string]any, error) {
	if triggerEventID == "" {
		return nil, errors.New("store: EventsByTriggerID needs a trigger_event_id")
	}
	rows, err := db.reader.QueryContext(ctx,
		selectEventsSQL+` WHERE trigger_event_id = ? ORDER BY ts, event_id`, triggerEventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var r eventRow
		if err := r.scan(rows); err != nil {
			return nil, err
		}
		obj, err := r.eventJSON()
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, rows.Err()
}

// EventByID reads one event as its canonical JSON object, for `standpipe trace`
// and the post-setup smoke check.
func (db *DB) EventByID(ctx context.Context, eventID string) (map[string]any, error) {
	var r eventRow
	err := r.scan(db.reader.QueryRowContext(ctx, selectEventsSQL+` WHERE event_id = ?`, eventID))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.eventJSON()
}
