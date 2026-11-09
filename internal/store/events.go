package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// schemaVersionOfEvents is the per-row event schema_version. It is bumped
// when the event JSON shape changes, independently of the DDL migration
// version, so an archive can carry rows of several versions side by side.
const schemaVersionOfEvents = 1

const defaultOriginForge = "github"

// eventInsert accumulates the columns one event type actually sets.
//
// The previous shape was a single 32-column positional INSERT with a `...any`
// argument list at every call site, padded with runs of `nil`. Adding a column
// there is not a compile error anywhere — it is a runtime error at whichever
// call site was missed, and the `correlation` column is the second column this
// table has gained. Naming the columns per event type makes a new column a
// local edit in the one writer that owns it, and a wrong column name fails
// immediately and loudly on every insert instead of silently shifting values.
type eventInsert struct {
	cols []string
	vals []any
}

func (e *eventInsert) set(col string, v any) *eventInsert {
	e.cols = append(e.cols, col)
	e.vals = append(e.vals, v)
	return e
}

// envelope sets the columns every event type carries.
func newEventInsert(eventType EventType, eventID string, ts time.Time, originForge string, epoch int64, sum string) *eventInsert {
	e := &eventInsert{}
	return e.
		set("event_id", eventID).
		set("schema_version", schemaVersionOfEvents).
		set("event_type", string(eventType)).
		set("ts", ts.UnixMilli()).
		set("origin_forge", originForge).
		set("epoch", epoch).
		set("content_sha256", sum)
}

func (e *eventInsert) exec(ctx context.Context, tx *sql.Conn) error {
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(e.cols)), ", ")
	q := "INSERT INTO trigger_events (" + strings.Join(e.cols, ", ") + ") VALUES (" + placeholders + ")"
	_, err := tx.ExecContext(ctx, q, e.vals...)
	return err
}

// nullable maps the empty string to a SQL NULL. Every nullable TEXT column in
// the events table is either absent or meaningful; "" is never a meaningful value there,
// and storing it would make the CHECK constraints and the export ambiguous.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(i int) any {
	if i == 0 {
		return nil
	}
	return int64(i)
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func originForge(s string) string {
	if s == "" {
		return defaultOriginForge
	}
	return s
}

// triggerHashScope builds the trigger event's closed field list (see
// docs/reference/hash-scopes.md). content_sha256 itself
// and the redactable fields (caller.source_ip, caller.user_agent, reason_text)
// are excluded, which is what lets `standpipe forget` NULL them without
// invalidating the hash.
func triggerHashScope(ev TriggerEvent) map[string]any {
	var requireRaw any
	if ev.RequireRaw != nil {
		requireRaw = *ev.RequireRaw
	}
	return map[string]any{
		"schema_version": int64(schemaVersionOfEvents),
		"event_type":     string(EventTrigger),
		"event_id":       ev.EventID,
		"ts":             ev.TS.UnixMilli(),
		"origin_forge":   originForge(ev.OriginForge),
		"epoch":          ev.Epoch,
		"caller": map[string]any{
			"token_id": ev.CallerTokenID,
			"name":     ev.CallerName,
		},
		"target": map[string]any{
			"project":  ev.TargetProject,
			"workflow": ev.TargetWorkflow,
			"ref":      ev.TargetRef,
		},
		"inputs_hash":       nullable(ev.InputsHash),
		"inputs_validation": nullable(string(ev.InputsValidation)),
		// correlation is IN the scope: it is a claim about what
		// the gateway armed this dispatch with, decided before the row is
		// written and never derivable from the log afterwards.
		"correlation":         nullable(string(ev.Correlation)),
		"idempotency_key":     ev.IdempotencyKey,
		"idempotency_outcome": nullable(string(ev.IdempotencyOutcome)),
		"require_raw":         requireRaw,
		"decision": map[string]any{
			"backend": nullable(ev.DecisionBackend),
			"reason":  nullable(ev.DecisionReason),
		},
		// A trigger row's outcome is always NULL (it is written with the
		// reservation, before any dispatch) and is never
		// updated, so the scope value is a stable null.
		"outcome":    nil,
		"request_id": nullable(ev.RequestID),
	}
}

// triggerOutcomeHashScope is the trigger_outcome closed list. There are no redactable
// fields on this event type and the row is final the moment it is written.
//
// origin_forge and epoch are read FROM THE EVENT, not hardcoded.
// Fixing them at "github"/0 gave the right answer for every row Phase 0 writes,
// but it also left those two columns outside this event type's integrity scope
// altogether: an archive that changed either would verify clean. The archive
// envelope put both
// columns there precisely so a later phase can carry other values, so the
// answer here is to widen the scope rather than to freeze the columns with a
// CHECK — the opposite of the trigger row's `outcome`, where the constant
// NULL is the design.
func triggerOutcomeHashScope(ev TriggerOutcomeEvent) map[string]any {
	return map[string]any{
		"schema_version":               int64(schemaVersionOfEvents),
		"event_type":                   string(EventTriggerOutcome),
		"event_id":                     ev.EventID,
		"ts":                           ev.TS.UnixMilli(),
		"origin_forge":                 originForge(ev.OriginForge),
		"epoch":                        ev.Epoch,
		"trigger_event_id":             ev.TriggerEventID,
		"outcome_status":               string(ev.OutcomeStatus),
		"outcome_reason":               nullable(ev.OutcomeReason),
		"outcome_http_status":          nullableInt(ev.OutcomeHTTPStatus),
		"outcome_upstream_duration_ms": nullableInt(int(ev.UpstreamDuration.Milliseconds())),
	}
}

// triggerRejectedHashScope is the trigger_rejected closed list. caller.source_ip and
// caller.user_agent are excluded for the same reason as on a trigger row: they
// are redactable. suppressed_* ARE in scope — the row is final the moment it is
// written, because the counter accumulates in memory and is written once when
// the window closes.
func triggerRejectedHashScope(ev TriggerRejectedEvent) map[string]any {
	return map[string]any{
		"schema_version": int64(schemaVersionOfEvents),
		"event_type":     string(EventTriggerRejected),
		"event_id":       ev.EventID,
		"ts":             ev.TS.UnixMilli(),
		"origin_forge":   originForge(ev.OriginForge),
		"epoch":          ev.Epoch,
		"caller": map[string]any{
			"token_id": ev.CallerTokenID,
			"name":     ev.CallerName,
		},
		"target": map[string]any{
			"project":  nullable(ev.TargetProject),
			"workflow": nullable(ev.TargetWorkflow),
			"ref":      nullable(ev.TargetRef),
		},
		"reject_reason":       string(ev.RejectReason),
		"outcome_http_status": int64(ev.HTTPStatus),
		"suppressed_count":    nullableInt(ev.SuppressedCount),
		"suppressed_since":    nullableTime(ev.SuppressedSince),
		"request_id":          nullable(ev.RequestID),
	}
}

func runLinkHashScope(ev RunLinkEvent) map[string]any {
	var runID any
	if ev.RunID != nil {
		runID = *ev.RunID
	}
	return map[string]any{
		"schema_version":   int64(schemaVersionOfEvents),
		"event_type":       string(EventRunLink),
		"event_id":         ev.EventID,
		"ts":               ev.TS.UnixMilli(),
		"trigger_event_id": ev.TriggerEventID,
		"run_id":           runID,
		"resolution":       string(ev.Resolution),
		// The run_link computes its own hash at its own write time, so
		// putting resolution_source in scope does not touch the immutability of
		// the trigger row.
		"resolution_source": string(ev.ResolutionSource),
		"origin_forge":      originForge(ev.OriginForge),
		"epoch":             ev.Epoch,
	}
}

func runObservedHashScope(ev RunObservedEvent) map[string]any {
	return map[string]any{
		"schema_version":   int64(schemaVersionOfEvents),
		"event_type":       string(EventRunObserved),
		"event_id":         ev.EventID,
		"ts":               ev.TS.UnixMilli(),
		"trigger_event_id": ev.TriggerEventID,
		"run_id":           ev.RunID,
		"conclusion":       ev.Conclusion,
		"origin_forge":     originForge(ev.OriginForge),
		"epoch":            ev.Epoch,
	}
}

// insertTrigger writes a `trigger` row. It takes a live connection because
// it must share the reservation's transaction — a
// separate transaction would mean a dangling event_id on the idempotency row
// if the process died between the two commits.
func insertTrigger(ctx context.Context, tx *sql.Conn, ev TriggerEvent) error {
	if ev.EventID == "" {
		return errors.New("store: trigger event needs an event_id")
	}
	if !ev.Correlation.valid() {
		// correlation is NOT NULL on a trigger row. Refused here
		// rather than left to the CHECK so the caller gets a legible message,
		// and never defaulted: the value is a claim about what this dispatch
		// was armed with, and inventing one forges the log.
		return fmt.Errorf("store: trigger event %s needs a correlation (injected|dispatch_response|window), got %q", ev.EventID, ev.Correlation)
	}
	sum, err := contentSHA256(triggerHashScope(ev))
	if err != nil {
		return err
	}
	var requireRaw any
	if ev.RequireRaw != nil {
		requireRaw = *ev.RequireRaw
	}
	ins := newEventInsert(EventTrigger, ev.EventID, ev.TS, originForge(ev.OriginForge), ev.Epoch, sum).
		set("caller_token_id", nullable(ev.CallerTokenID)).
		set("caller_name", nullable(ev.CallerName)).
		set("caller_source_ip", nullable(ev.CallerSourceIP)).
		set("caller_user_agent", nullable(ev.CallerUserAgent)).
		set("target_project", nullable(ev.TargetProject)).
		set("target_workflow", nullable(ev.TargetWorkflow)).
		set("target_ref", nullable(ev.TargetRef)).
		set("inputs_hash", nullable(ev.InputsHash)).
		set("inputs_validation", nullable(string(ev.InputsValidation))).
		set("correlation", string(ev.Correlation)).
		set("idempotency_key", nullable(ev.IdempotencyKey)).
		set("idempotency_outcome", nullable(string(ev.IdempotencyOutcome))).
		set("require_raw", requireRaw).
		set("decision_backend", nullable(ev.DecisionBackend)).
		set("decision_reason", nullable(ev.DecisionReason)).
		set("reason_text", nullable(ev.ReasonText)).
		set("request_id", nullable(ev.RequestID))
	// outcome_* are deliberately not set: a trigger row's outcome is NULL
	// and lives in a separate trigger_outcome event.
	if err := ins.exec(ctx, tx); err != nil {
		return fmt.Errorf("store: writing trigger event %s: %w", ev.EventID, err)
	}
	return nil
}

func insertTriggerOutcome(ctx context.Context, tx *sql.Conn, ev TriggerOutcomeEvent) error {
	if ev.EventID == "" {
		return errors.New("store: trigger_outcome event needs an event_id")
	}
	if ev.TriggerEventID == "" {
		return errors.New("store: trigger_outcome event needs a trigger_event_id")
	}
	switch ev.OutcomeStatus {
	case OutcomeForwarded, OutcomeRejected, OutcomeError:
	default:
		return fmt.Errorf("store: invalid outcome_status %q", ev.OutcomeStatus)
	}
	sum, err := contentSHA256(triggerOutcomeHashScope(ev))
	if err != nil {
		return err
	}
	// The envelope is written from the event, and the scope above hashes the
	// same two values from the same place, so the column and the hash can no
	// longer disagree.
	ins := newEventInsert(EventTriggerOutcome, ev.EventID, ev.TS, originForge(ev.OriginForge), ev.Epoch, sum).
		set("outcome_status", string(ev.OutcomeStatus)).
		set("outcome_reason", nullable(ev.OutcomeReason)).
		set("outcome_http_status", nullableInt(ev.OutcomeHTTPStatus)).
		set("outcome_upstream_duration_ms", nullableInt(int(ev.UpstreamDuration.Milliseconds()))).
		set("trigger_event_id", ev.TriggerEventID)
	if err := ins.exec(ctx, tx); err != nil {
		return fmt.Errorf("store: writing trigger_outcome event %s: %w", ev.EventID, err)
	}
	return nil
}

func insertTriggerRejected(ctx context.Context, tx *sql.Conn, ev TriggerRejectedEvent) error {
	if ev.EventID == "" {
		return errors.New("store: trigger_rejected event needs an event_id")
	}
	if !ev.RejectReason.valid() {
		return fmt.Errorf("store: invalid reject_reason %q", ev.RejectReason)
	}
	switch ev.HTTPStatus {
	case 400, 403, 422, 429:
	default:
		return fmt.Errorf("store: trigger_rejected http status must be 400, 403, 422 or 429, got %d", ev.HTTPStatus)
	}
	// suppressed_* belong to the coalesced 429 row and nowhere else.
	hasSuppression := ev.SuppressedCount != 0 || !ev.SuppressedSince.IsZero()
	if hasSuppression {
		if ev.RejectReason != RejectRateLimited {
			return fmt.Errorf("store: suppressed_count/suppressed_since are only valid on reject_reason=rate_limited, got %q", ev.RejectReason)
		}
		if ev.SuppressedCount < 1 || ev.SuppressedSince.IsZero() {
			return errors.New("store: a coalesced 429 needs suppressed_count >= 1 and a suppressed_since")
		}
	}
	sum, err := contentSHA256(triggerRejectedHashScope(ev))
	if err != nil {
		return err
	}
	ins := newEventInsert(EventTriggerRejected, ev.EventID, ev.TS, originForge(ev.OriginForge), ev.Epoch, sum).
		set("caller_token_id", nullable(ev.CallerTokenID)).
		set("caller_name", nullable(ev.CallerName)).
		set("caller_source_ip", nullable(ev.CallerSourceIP)).
		set("caller_user_agent", nullable(ev.CallerUserAgent)).
		set("target_project", nullable(ev.TargetProject)).
		set("target_workflow", nullable(ev.TargetWorkflow)).
		set("target_ref", nullable(ev.TargetRef)).
		set("reject_reason", string(ev.RejectReason)).
		set("outcome_http_status", int64(ev.HTTPStatus)).
		set("suppressed_count", nullableInt(ev.SuppressedCount)).
		set("suppressed_since", nullableTime(ev.SuppressedSince)).
		set("reason_text", nullable(ev.ReasonText)).
		set("request_id", nullable(ev.RequestID))
	// trigger_event_id stays unset: the event is parentless and the
	// table-level CHECK enforces it.
	if err := ins.exec(ctx, tx); err != nil {
		return fmt.Errorf("store: writing trigger_rejected event %s: %w", ev.EventID, err)
	}
	return nil
}

func insertRunLink(ctx context.Context, tx *sql.Conn, ev RunLinkEvent) error {
	sum, err := contentSHA256(runLinkHashScope(ev))
	if err != nil {
		return err
	}
	var runID any
	if ev.RunID != nil {
		runID = *ev.RunID
	}
	ins := newEventInsert(EventRunLink, ev.EventID, ev.TS, originForge(ev.OriginForge), ev.Epoch, sum).
		set("trigger_event_id", ev.TriggerEventID).
		set("run_id", runID).
		set("resolution", string(ev.Resolution)).
		set("resolution_source", string(ev.ResolutionSource))
	if err := ins.exec(ctx, tx); err != nil {
		return fmt.Errorf("store: writing run_link event %s: %w", ev.EventID, err)
	}
	return nil
}

func insertRunObserved(ctx context.Context, tx *sql.Conn, ev RunObservedEvent) error {
	sum, err := contentSHA256(runObservedHashScope(ev))
	if err != nil {
		return err
	}
	ins := newEventInsert(EventRunObserved, ev.EventID, ev.TS, originForge(ev.OriginForge), ev.Epoch, sum).
		set("trigger_event_id", ev.TriggerEventID).
		set("run_id", ev.RunID).
		set("conclusion", nullable(ev.Conclusion))
	if err := ins.exec(ctx, tx); err != nil {
		return fmt.Errorf("store: writing run_observed event %s: %w", ev.EventID, err)
	}
	return nil
}

// AppendTriggerRejected records an authenticated request that was refused
// before any reservation was taken (rate limit, caller policy, allowlist or
// request validation).
//
// The event is parentless: no idempotency row, no trigger row, no
// trigger_event_id. That is the whole point. Writing a trigger +
// trigger_outcome pair instead — which is what this package used to do — puts a
// request that forwarded nothing into the resolver's work queue and
// makes it countable in the coverage join; the two representations
// cannot coexist without double counting.
//
// Requests refused BEFORE authentication are still not written at all: an
// unauthenticated request cannot be attributed to a caller.
//
// A 429 must arrive here coalesced: one row per caller per
// rate_limit.reject_log_window, carrying SuppressedCount and SuppressedSince.
// Writing a row per rejected request would turn the rate limiter into a write
// amplifier against the single writer, which is the problem the limiter exists
// to prevent.
func (db *DB) AppendTriggerRejected(ctx context.Context, ev TriggerRejectedEvent) (string, error) {
	if ev.EventID == "" {
		ev.EventID = db.cfg.newEventID()
	}
	if ev.TS.IsZero() {
		ev.TS = db.cfg.now()
	}
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		return insertTriggerRejected(ctx, tx, ev)
	})
	if err != nil {
		return "", err
	}
	return ev.EventID, nil
}

// AppendRejectedTrigger writes the trigger_rejected event from the older
// trigger + outcome argument shape.
//
// Deprecated: it exists only so that callers written against the old
// trigger + trigger_outcome representation keep compiling. It no longer writes
// that pair — it was replaced by the parentless trigger_rejected event — and
// it requires outcome.OutcomeReason to name a trigger_rejected reject_reason, because the
// old shape carried no field that could supply one. Call
// AppendTriggerRejected directly.
func (db *DB) AppendRejectedTrigger(ctx context.Context, ev TriggerEvent, outcome TriggerOutcomeEvent) error {
	if outcome.OutcomeStatus == OutcomeForwarded {
		return errors.New("store: AppendRejectedTrigger cannot record outcome_status=forwarded")
	}
	reason := RejectReason(outcome.OutcomeReason)
	if !reason.valid() {
		return fmt.Errorf("store: AppendRejectedTrigger now writes a %s event; "+
			"outcome_reason %q is not one of its reject_reason values — use AppendTriggerRejected",
			EventTriggerRejected, outcome.OutcomeReason)
	}
	_, err := db.AppendTriggerRejected(ctx, TriggerRejectedEvent{
		EventID:         ev.EventID,
		TS:              ev.TS,
		OriginForge:     ev.OriginForge,
		Epoch:           ev.Epoch,
		CallerTokenID:   ev.CallerTokenID,
		CallerName:      ev.CallerName,
		CallerSourceIP:  ev.CallerSourceIP,
		CallerUserAgent: ev.CallerUserAgent,
		TargetProject:   ev.TargetProject,
		TargetWorkflow:  ev.TargetWorkflow,
		TargetRef:       ev.TargetRef,
		RejectReason:    reason,
		HTTPStatus:      outcome.OutcomeHTTPStatus,
		ReasonText:      ev.ReasonText,
		RequestID:       ev.RequestID,
	})
	return err
}

// writeRunLink applies the run_link uniqueness rule inside an already-open write
// transaction: at most one active run_link per trigger, and a new resolution is
// only accepted when it is strictly more reliable than the active one
// (none < ambiguous < inferred < exact).
//
// The supersede is expressed append-only: the old row keeps its content and
// gains a superseded_by pointer, and the new resolution arrives as a new row.
// Both happen in the caller's transaction, so the partial UNIQUE index never
// sees two active rows.
//
// This is the authority for supersession. The resolver's
// own ExistingResolution check is an optimisation: two concurrent sweeps can
// both pass it, and only this transaction decides.
func writeRunLink(ctx context.Context, tx *sql.Conn, ev RunLinkEvent) error {
	var oldID, oldRes string
	err := tx.QueryRowContext(ctx, `
		SELECT event_id, resolution FROM trigger_events
		WHERE event_type = 'run_link' AND superseded_by IS NULL AND trigger_event_id = ?`,
		ev.TriggerEventID).Scan(&oldID, &oldRes)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no active link yet
	case err != nil:
		return err
	default:
		if Resolution(oldRes).rank() >= ev.Resolution.rank() {
			return ErrWeakerResolution
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE trigger_events SET superseded_by = ? WHERE event_id = ?`,
			ev.EventID, oldID); err != nil {
			return err
		}
	}
	return insertRunLink(ctx, tx, ev)
}

// validateRunLink checks everything that does not need the database.
func validateRunLink(ev RunLinkEvent) error {
	if ev.TriggerEventID == "" {
		return errors.New("store: run_link needs a trigger_event_id")
	}
	if !ev.Resolution.valid() {
		return fmt.Errorf("store: invalid resolution %q", ev.Resolution)
	}
	if !ev.ResolutionSource.valid() {
		// NOT NULL on a run_link row. Not defaulted — "which
		// layer established this link" is the question the correlation adoption report
		// and `standpipe trace` exist to answer, and no default can answer it.
		return fmt.Errorf("store: run_link needs a resolution_source (dispatch_response|run_name|window), got %q", ev.ResolutionSource)
	}
	if ev.Resolution == ResolutionNone && ev.RunID != nil {
		return errors.New("store: resolution=none must carry no run_id")
	}
	if ev.Resolution != ResolutionNone && ev.RunID == nil {
		return fmt.Errorf("store: resolution=%s needs a run_id", ev.Resolution)
	}
	return nil
}

// AppendRunLink writes one run_link in its own transaction, applying the
// run_link supersede rule. It is the background resolver's entry point
// (correlation layers 2 and 3).
//
// The layer-1 link written right after the dispatch does NOT come through here:
// it must share the completion's transaction, so Complete carries it.
// Otherwise a state exists where the dispatch succeeded and the run id was lost.
//
// It returns ErrWeakerResolution when the existing link is at least as strong.
// That is an ordinary outcome, not a failure.
func (db *DB) AppendRunLink(ctx context.Context, ev RunLinkEvent) (string, error) {
	if err := validateRunLink(ev); err != nil {
		return "", err
	}
	if ev.EventID == "" {
		ev.EventID = db.cfg.newEventID()
	}
	if ev.TS.IsZero() {
		ev.TS = db.cfg.now()
	}
	if err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		return writeRunLink(ctx, tx, ev)
	}); err != nil {
		return "", err
	}
	return ev.EventID, nil
}

// AppendRunObserved records a run's conclusion. It is written by the
// inventory refresh when a conclusion changes on a run that has an active
// run_link.
func (db *DB) AppendRunObserved(ctx context.Context, ev RunObservedEvent) (string, error) {
	if ev.TriggerEventID == "" {
		return "", errors.New("store: run_observed needs a trigger_event_id")
	}
	if ev.Conclusion == "" {
		return "", errors.New("store: run_observed needs a conclusion")
	}
	if ev.EventID == "" {
		ev.EventID = db.cfg.newEventID()
	}
	if ev.TS.IsZero() {
		ev.TS = db.cfg.now()
	}
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		return insertRunObserved(ctx, tx, ev)
	})
	if err != nil {
		return "", err
	}
	return ev.EventID, nil
}

// redactableEventTypes are the rows that carry caller identity and are
// therefore in scope for `standpipe forget`.
//
// trigger_rejected is in the list: it carries the same caller block as a
// trigger row. Redacting only `trigger` leaves the source IPs of every
// rejected request behind — a silent compliance gap, and exactly the trap
// that awaits any query that enumerates event types literally.
const redactableEventTypes = `('trigger', 'trigger_rejected')`

const forgetPredicate = `
 WHERE event_type IN ` + redactableEventTypes + `
   AND ts < ?
   AND (caller_source_ip IS NOT NULL
     OR caller_user_agent IS NOT NULL
     OR reason_text IS NOT NULL)`

// Forget NULLs the redactable fields of every trigger and trigger_rejected
// event older than before — `standpipe forget`.
// content_sha256 stays valid because those fields were never in either hash
// scope.
func (db *DB) Forget(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE trigger_events
			   SET caller_source_ip = NULL, caller_user_agent = NULL, reason_text = NULL`+
			forgetPredicate, before.UnixMilli())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// ForgetPlan counts what Forget would change, for `standpipe forget --dry-run`.
func (db *DB) ForgetPlan(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := db.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM trigger_events`+forgetPredicate, before.UnixMilli()).Scan(&n)
	return n, err
}
