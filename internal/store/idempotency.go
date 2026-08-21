package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReserveOutcome is the decision the reservation protocol reached.
type ReserveOutcome string

const (
	// ReserveNew — the row was created by this attempt. The caller owns the
	// key and may dispatch.
	ReserveNew ReserveOutcome = "new"

	// ReserveReclaimed — an `unknown` row matching on body and path was taken
	// over by this attempt's CAS. The caller owns the key, but the replay rules require
	// the run-link resolver to run against the original time window FIRST:
	// if a matching run already exists the record is completed as
	// forwarded and the dispatch is NOT repeated.
	ReserveReclaimed ReserveOutcome = "reclaimed"

	// ReserveReplay — a complete row with the same body and path exists. The
	// caller returns the stored response with idempotency: "replay".
	ReserveReplay ReserveOutcome = "replay"

	// ReserveInProgress — another attempt holds the key. 409
	// idempotency_in_progress + Retry-After: 5. NEVER dispatch.
	ReserveInProgress ReserveOutcome = "in_progress"

	// ReserveConflict — same key, different body or different path. 409
	// idempotency_conflict.
	ReserveConflict ReserveOutcome = "conflict"
)

// MayDispatch reports whether this outcome permits a dispatch to GitHub.
//
// It is false for ReserveInProgress by construction: the whole point of the
// reservation protocol
// is that a second request against an in-flight key is never forwarded. It is
// true for ReserveReclaimed only in the sense that the caller owns the key —
// the resolver check against the original window still comes first.
func (o ReserveOutcome) MayDispatch() bool {
	return o == ReserveNew || o == ReserveReclaimed
}

// ConflictReason distinguishes the two 409 idempotency_conflict causes so the
// gateway can put an honest `detail` in the response.
type ConflictReason string

const (
	ConflictNone ConflictReason = ""
	// ConflictBody — same key, different canonical body.
	ConflictBody ConflictReason = "body_mismatch"
	// ConflictPath — same key, same body, different METHOD + path.
	ConflictPath ConflictReason = "path_mismatch"
)

// ReserveRequest is one attempt to take the key (caller_name, idempotency_key).
type ReserveRequest struct {
	// CallerName is the idempotency namespace. Token rotation must not
	// change it; caller_token_id is audit trail only.
	CallerName string
	// CallerTokenID is recorded for audit and is not part of the key.
	CallerTokenID string
	// IdempotencyKey is the caller's opaque header value, stored verbatim.
	IdempotencyKey string
	// RequestPath is METHOD + path, e.g. "POST /t/acme/core/release.yml".
	RequestPath string
	// BodySHA256 is the 32-byte digest from BodySHA256().
	BodySHA256 []byte
	// TTL is gateway.idempotency_ttl, default 24h at the config layer.
	TTL time.Duration

	// Event is the `trigger` event written in the SAME transaction as the
	// reservation. Its EventID becomes the row's event_id, so
	// event_id is never dangling. On ReserveReclaimed this event id replaces
	// the old one on the row, per the reservation CAS.
	Event TriggerEvent
}

// ReserveResult is the outcome plus whatever the caller needs to answer.
type ReserveResult struct {
	Outcome ReserveOutcome

	// EventID is the trigger event this attempt owns (New, Reclaimed), or the
	// event id recorded on the existing row (Replay, InProgress, Conflict).
	EventID string

	// ResponseStatus and ResponseBody are the cached response, set only on
	// ReserveReplay.
	ResponseStatus int
	ResponseBody   string

	// ConflictReason is set only on ReserveConflict.
	ConflictReason ConflictReason

	// PriorState is the state found on the existing row, empty on ReserveNew.
	PriorState State

	// OriginalAttemptAt is the attempt_started_at of the row before this CAS
	// took it over, set only on ReserveReclaimed. The replay path needs it: the resolver
	// is run against the ORIGINAL dispatch window, not against now.
	OriginalAttemptAt time.Time
	// OriginalDispatchAt is the moment the most recent attempt that got that
	// far began its dispatch, and nil ONLY when no attempt on this key ever
	// reached the dispatch stage. Set on ReserveReclaimed.
	//
	// It is not merely the superseded row's dispatch_started_at: that column
	// is the current attempt's liveness clock and the CAS below has to clear
	// it (the crash-recovery sweep measures a non-NULL value against
	// upstream_timeout). The fact
	// is carried in prior_dispatch_started_at instead, so a CHAIN of reclaims
	// — attempt 1 posts, attempt 2 dies before its own stamp — does not report
	// "nothing was ever posted" to the caller. internal/forge treats nil as a
	// definitive negative for correlation tier 3, so the distinction decides whether
	// a duplicate dispatch is caught.
	OriginalDispatchAt *time.Time

	// OriginalEventID is the trigger event id the SUPERSEDED attempt carried,
	// set only on ReserveReclaimed. It is the value that was injected as
	// `standby_trigger_id` into the dispatch that timed out, and therefore the
	// only id a run name can carry (correlation tier 2).
	//
	// EventID above is this attempt's NEW event, which the CAS has just
	// written over idempotency.event_id; it was never injected anywhere.
	// Searching run names for it can only miss, which is why the two are
	// separate fields and why forge.ResolveWindow.OriginalEventID must be
	// filled from THIS one. Empty there disables the exact tier and is
	// reported as Tier2OriginalIDUnavailable — a degradation the caller can
	// see, rather than a search guaranteed to fail for a reason nothing
	// records.
	OriginalEventID string
}

const reserveInsertSQL = `
INSERT INTO idempotency (
    caller_name, idempotency_key, caller_token_id, request_path, body_sha256,
    state, response_status, response_body, event_id,
    attempt_started_at, dispatch_started_at, prior_dispatch_started_at,
    created_at, expires_at
) VALUES (
    ?, ?, ?, ?, ?,
    'in_flight', NULL, NULL, ?,
    ?, NULL, NULL,
    ?, ?
)
ON CONFLICT(caller_name, idempotency_key) DO NOTHING`

// Reserve takes the idempotency key before any dispatch. The whole
// protocol runs inside one BEGIN IMMEDIATE:
//
//	INSERT ... ON CONFLICT DO NOTHING
//	changes() == 1 -> new reservation, write the trigger event, dispatch
//	changes() == 0 -> read the row and apply the reservation decision table
//
// The trigger event is written in this same transaction. Two
// transactions would allow a reservation whose event_id points at nothing —
// a dispatch with no record, which is worse than no dispatch.
//
// Reserve writes NO event for Replay, InProgress or Conflict. Those requests
// took no reservation and forwarded nothing; a trigger row for them would
// enter the resolver queue and the coverage join as if a run had
// been dispatched. The counters gateway_idempotency_replay_total and
// gateway_idempotency_conflict_total are the surface for those.
// Rejections that happen BEFORE the reservation are a different class and have
// their own parentless event: AppendTriggerRejected.
func (db *DB) Reserve(ctx context.Context, req ReserveRequest) (ReserveResult, error) {
	if req.CallerName == "" {
		return ReserveResult{}, errors.New("store: Reserve needs a caller_name")
	}
	if req.IdempotencyKey == "" {
		return ReserveResult{}, errors.New("store: Reserve needs an idempotency_key")
	}
	if req.RequestPath == "" {
		return ReserveResult{}, errors.New("store: Reserve needs a request_path")
	}
	if len(req.BodySHA256) != 32 {
		return ReserveResult{}, fmt.Errorf("store: Reserve needs a 32-byte body_sha256, got %d", len(req.BodySHA256))
	}
	if req.TTL <= 0 {
		return ReserveResult{}, errors.New("store: Reserve needs a positive TTL")
	}
	if req.Event.EventID == "" {
		return ReserveResult{}, errors.New("store: Reserve needs Event.EventID")
	}

	now := db.cfg.now()
	nowMS := now.UnixMilli()
	ev := req.Event
	if ev.TS.IsZero() {
		ev.TS = now
	}
	ev.IdempotencyKey = req.IdempotencyKey
	ev.IdempotencyOutcome = IdempotencyNew
	if ev.CallerName == "" {
		ev.CallerName = req.CallerName
	}
	if ev.CallerTokenID == "" {
		ev.CallerTokenID = req.CallerTokenID
	}

	var out ReserveResult
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		res, err := tx.ExecContext(ctx, reserveInsertSQL,
			req.CallerName, req.IdempotencyKey, req.CallerTokenID, req.RequestPath, req.BodySHA256,
			ev.EventID,
			nowMS, nowMS, nowMS+req.TTL.Milliseconds(),
		)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			out = ReserveResult{Outcome: ReserveNew, EventID: ev.EventID}
			return insertTrigger(ctx, tx, ev)
		}

		// changes() == 0: someone else owns the key. Read it and apply the
		// decision table.
		rec, err := scanIdempotency(tx.QueryRowContext(ctx, selectIdempotencySQL, req.CallerName, req.IdempotencyKey))
		if err != nil {
			return err
		}

		bodyMatches := bytes.Equal(rec.BodySHA256, req.BodySHA256)
		pathMatches := rec.RequestPath == req.RequestPath
		out.EventID = rec.EventID
		out.PriorState = rec.State

		switch rec.State {
		case StateComplete:
			switch {
			case !bodyMatches:
				out.Outcome, out.ConflictReason = ReserveConflict, ConflictBody
			case !pathMatches:
				out.Outcome, out.ConflictReason = ReserveConflict, ConflictPath
			default:
				out.Outcome = ReserveReplay
				out.ResponseStatus = rec.ResponseStatus
				out.ResponseBody = rec.ResponseBody
			}
			return nil

		case StateInFlight:
			// Never dispatch, whatever the body says. A body mismatch
			// here is still answered in_progress, because the honest answer
			// is "we cannot tell you yet" — the row may still become a
			// complete row with the other body.
			out.Outcome = ReserveInProgress
			return nil

		case StateUnknown:
			switch {
			case !bodyMatches:
				out.Outcome, out.ConflictReason = ReserveConflict, ConflictBody
				return nil
			case !pathMatches:
				// Not spelled out in the reservation decision table, which only names the body
				// column for `unknown`. The CAS below requires request_path to
				// match, so a path mismatch can never take the row; reporting
				// it as a conflict keeps it consistent with the `complete`
				// rows two lines up.
				out.Outcome, out.ConflictReason = ReserveConflict, ConflictPath
				return nil
			}

			// Retry path: compare-and-swap. changes() == 0 means another
			// attempt took the row first -> 409 idempotency_in_progress.
			//
			// dispatch_started_at is cleared because the crash-recovery sweep reads it as THIS
			// attempt's liveness clock: a non-NULL value means "inside a
			// GitHub call" and is measured against upstream_timeout. Leaving
			// the dead attempt's stamp there would put the new attempt past
			// that cutoff before it had begun, and the sweep would flip a live
			// reservation on its next tick.
			//
			// The fact that somebody posted is not lost with it: it moves to
			// prior_dispatch_started_at, and COALESCE carries it across a
			// chain of reclaims rather than only one hop.
			upd, err := tx.ExecContext(ctx, `
				UPDATE idempotency
				   SET state = 'in_flight', attempt_started_at = ?,
				       prior_dispatch_started_at =
				           COALESCE(dispatch_started_at, prior_dispatch_started_at),
				       dispatch_started_at = NULL, event_id = ?,
				       response_status = NULL, response_body = NULL,
				       caller_token_id = ?
				 WHERE caller_name = ? AND idempotency_key = ?
				   AND state = 'unknown' AND body_sha256 = ? AND request_path = ?`,
				nowMS, ev.EventID, req.CallerTokenID,
				req.CallerName, req.IdempotencyKey, req.BodySHA256, req.RequestPath)
			if err != nil {
				return err
			}
			changed, err := upd.RowsAffected()
			if err != nil {
				return err
			}
			if changed == 0 {
				out.Outcome = ReserveInProgress
				return nil
			}
			out.Outcome = ReserveReclaimed
			// rec is the row as it stood BEFORE the CAS, so rec.EventID is
			// the id the superseded attempt injected. out.EventID is about to
			// be overwritten with this attempt's new id, which was never
			// injected anywhere — capture the old one first (correlation tier 2).
			out.OriginalEventID = rec.EventID
			out.EventID = ev.EventID
			out.OriginalAttemptAt = rec.AttemptStartedAt
			// The superseded attempt's own stamp when it had one, otherwise
			// whatever an earlier attempt in the chain left behind. Same
			// COALESCE as the UPDATE above, and for the same reason: nil here
			// is read downstream as "nothing was ever posted".
			if rec.DispatchStartedAt != nil {
				out.OriginalDispatchAt = rec.DispatchStartedAt
			} else {
				out.OriginalDispatchAt = rec.PriorDispatchStartedAt
			}
			// The row's event_id now points at this attempt's event, so the
			// event must exist in this same transaction.
			return insertTrigger(ctx, tx, ev)

		default:
			return fmt.Errorf("store: unexpected idempotency state %q", rec.State)
		}
	})
	if err != nil {
		return ReserveResult{}, err
	}
	return out, nil
}

// MarkDispatchStarted stamps dispatch_started_at.
//
// It MUST return before the request is sent to GitHub. The crash-recovery
// sweep measures a row with a NULL dispatch_started_at against
// max_queue_wait, which is seconds, and a row with a stamp against
// upstream_timeout. Dispatching first and stamping afterwards would let the
// sweeper flip a live in-flight row to `unknown` while the call is still
// running — the sweep race, reintroduced from the caller's side.
//
// It returns ErrNotOwner when the row is no longer in_flight under this
// event_id.
func (db *DB) MarkDispatchStarted(ctx context.Context, callerName, key, eventID string) error {
	nowMS := db.cfg.now().UnixMilli()
	return db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE idempotency SET dispatch_started_at = ?
			 WHERE caller_name = ? AND idempotency_key = ?
			   AND state = 'in_flight' AND event_id = ?`,
			nowMS, callerName, key, eventID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotOwner
		}
		return nil
	})
}

// CompleteRequest finishes an attempt.
type CompleteRequest struct {
	CallerName     string
	IdempotencyKey string
	// EventID is the trigger event this attempt owns. It guards the UPDATE.
	EventID string

	// FinalState is StateComplete for a deterministic result (2xx, upstream
	// 404, upstream 422) or StateUnknown for a timeout or 5xx.
	FinalState State

	// ResponseStatus and ResponseBody are the replayable response. They are
	// forced to NULL when FinalState is StateUnknown: the gateway caches no response
	// for a result the gateway could not observe.
	ResponseStatus int
	ResponseBody   string

	// Outcome is the trigger_outcome event, written in the same
	// transaction. Its TriggerEventID is set to EventID.
	Outcome TriggerOutcomeEvent

	// RunLink is the OPTIONAL tier-1 link: the dispatch
	// response carried a workflow_run_id, so the correlation is already
	// established on the request path. It is written in THIS transaction
	// — writing it separately leaves a window in which the
	// dispatch counts as successful and the run id is lost, and that trigger
	// then enters the resolver queue for nothing.
	//
	// TriggerEventID is set to EventID; Resolution defaults to exact and
	// ResolutionSource to dispatch_response when left empty, because those are
	// the only values correlation layer 1 can produce. Everything else is validated.
	RunLink *RunLinkEvent
}

// CompleteResult reports whether the idempotency row was actually updated.
type CompleteResult struct {
	// Applied is false when the guarded UPDATE matched no row: the sweep
	// or another retry took the record over while this dispatch was in
	// flight. The response is then NOT recorded — the protocol puts that guard there
	// precisely so a stale answer cannot overwrite a newer attempt.
	Applied bool
	// OutcomeEventID is the id of the trigger_outcome event that was written.
	OutcomeEventID string

	// RunLinkEventID is the id of the tier-1 run_link, empty when none was
	// requested or when it was not written.
	RunLinkEventID string
	// RunLinkApplied is false when a RunLink was supplied but an at-least-as-
	// strong link already existed. The completion still stands: the
	// dispatch really happened and its result belongs in the log. This is a
	// field rather than an error precisely so it cannot be lost silently.
	RunLinkApplied bool
}

// Complete records the dispatch result, appends the trigger_outcome event and,
// when the dispatch response carried a run id, the tier-1 run_link — all in one
// transaction.
//
// The trigger_outcome event is written even when Applied is false. The
// dispatch really happened and its result belongs in the append-only log,
// attached to the trigger event that caused it; only the idempotency row —
// the thing another attempt may now own — is left alone. Losing that record
// would hide a real GitHub call.
func (db *DB) Complete(ctx context.Context, req CompleteRequest) (CompleteResult, error) {
	if req.EventID == "" {
		return CompleteResult{}, errors.New("store: Complete needs an EventID")
	}
	switch req.FinalState {
	case StateComplete, StateUnknown:
	default:
		return CompleteResult{}, fmt.Errorf("store: Complete final state must be complete or unknown, got %q", req.FinalState)
	}

	outcome := req.Outcome
	outcome.TriggerEventID = req.EventID
	if outcome.EventID == "" {
		outcome.EventID = db.cfg.newEventID()
	}
	if outcome.TS.IsZero() {
		outcome.TS = db.cfg.now()
	}

	var link *RunLinkEvent
	if req.RunLink != nil {
		l := *req.RunLink
		l.TriggerEventID = req.EventID
		if l.Resolution == "" {
			l.Resolution = ResolutionExact
		}
		if l.ResolutionSource == "" {
			l.ResolutionSource = SourceDispatchResponse
		}
		if l.EventID == "" {
			l.EventID = db.cfg.newEventID()
		}
		if l.TS.IsZero() {
			l.TS = outcome.TS
		}
		if err := validateRunLink(l); err != nil {
			return CompleteResult{}, err
		}
		link = &l
	}

	var status, body any
	if req.FinalState == StateComplete {
		status, body = nullableInt(req.ResponseStatus), nullable(req.ResponseBody)
	}

	var out CompleteResult
	out.OutcomeEventID = outcome.EventID
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE idempotency
			   SET state = ?, response_status = ?, response_body = ?
			 WHERE caller_name = ? AND idempotency_key = ?
			   AND state = 'in_flight' AND event_id = ?`,
			string(req.FinalState), status, body,
			req.CallerName, req.IdempotencyKey, req.EventID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		out.Applied = n == 1
		if err := insertTriggerOutcome(ctx, tx, outcome); err != nil {
			return err
		}
		if link == nil {
			return nil
		}
		out.RunLinkEventID = link.EventID
		switch err := writeRunLink(ctx, tx, *link); {
		case err == nil:
			out.RunLinkApplied = true
			return nil
		case errors.Is(err, ErrWeakerResolution):
			// Not a failure and not a reason to lose the completion: the
			// existing link is at least as strong. Reported on the result.
			out.RunLinkApplied = false
			out.RunLinkEventID = ""
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return CompleteResult{}, err
	}
	return out, nil
}

const selectIdempotencySQL = `
SELECT caller_name, idempotency_key, caller_token_id, request_path, body_sha256,
       state, response_status, response_body, event_id,
       attempt_started_at, dispatch_started_at, prior_dispatch_started_at,
       created_at, expires_at
  FROM idempotency
 WHERE caller_name = ? AND idempotency_key = ?`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanIdempotency(row rowScanner) (IdempotencyRecord, error) {
	var (
		rec             IdempotencyRecord
		state           string
		status          sql.NullInt64
		body            sql.NullString
		attemptMS       int64
		dispatchMS      sql.NullInt64
		priorDispatchMS sql.NullInt64
		createdMS       int64
		expiresMS       int64
	)
	err := row.Scan(&rec.CallerName, &rec.IdempotencyKey, &rec.CallerTokenID, &rec.RequestPath,
		&rec.BodySHA256, &state, &status, &body, &rec.EventID,
		&attemptMS, &dispatchMS, &priorDispatchMS, &createdMS, &expiresMS)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, ErrNotFound
	}
	if err != nil {
		return rec, err
	}
	rec.State = State(state)
	rec.ResponseStatus = int(status.Int64)
	rec.ResponseBody = body.String
	rec.AttemptStartedAt = time.UnixMilli(attemptMS)
	if dispatchMS.Valid {
		t := time.UnixMilli(dispatchMS.Int64)
		rec.DispatchStartedAt = &t
	}
	if priorDispatchMS.Valid {
		t := time.UnixMilli(priorDispatchMS.Int64)
		rec.PriorDispatchStartedAt = &t
	}
	rec.CreatedAt = time.UnixMilli(createdMS)
	rec.ExpiresAt = time.UnixMilli(expiresMS)
	return rec, nil
}

// LookupIdempotency reads one record. It runs on the read pool and takes no
// lock, so it is safe for diagnostics while a dispatch is in flight. It is not
// part of the reservation protocol — Reserve does its own read inside the
// write transaction, because a read outside it would be a TOCTOU window.
func (db *DB) LookupIdempotency(ctx context.Context, callerName, key string) (IdempotencyRecord, error) {
	return scanIdempotency(db.reader.QueryRowContext(ctx, selectIdempotencySQL, callerName, key))
}
