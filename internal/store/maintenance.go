package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// SweepParams configures the crash-recovery sweep.
type SweepParams struct {
	// UpstreamTimeout is github.upstream_timeout, the ceiling on one
	// GitHub call.
	UpstreamTimeout time.Duration
	// MaxQueueWait is github.max_queue_wait, the ceiling on time spent
	// waiting for a slot before the call starts.
	MaxQueueWait time.Duration
	// Margin is added to both. It must comfortably exceed the sweep interval,
	// otherwise a row can cross the threshold and be swept in the same tick
	// that the dispatch is still legitimately running.
	Margin time.Duration
}

// SweepStaleInFlight flips abandoned in_flight rows to `unknown`. It
// runs periodically and is independent of graceful shutdown: rows that survive
// a hard kill are recovered here.
//
// The threshold is measured from dispatch_started_at when the dispatch
// started, and from attempt_started_at only while the request is still queued.
// Measuring everything from queue entry was an earlier bug: it flips a request
// that is genuinely in flight, because max_queue_wait is seconds while
// upstream_timeout is tens of seconds.
//
// Note what remains true even when the sweep is correct: a row flipped to
// `unknown` can be reclaimed by a retry while the original dispatch is still
// travelling. The original attempt's Complete then fails its guard and does
// not overwrite anything, but GitHub may have received the first dispatch.
// The replay rule is the mitigation — the retry runs the run-link resolver against the
// original window before dispatching again.
func (db *DB) SweepStaleInFlight(ctx context.Context, p SweepParams) (int64, error) {
	if p.UpstreamTimeout <= 0 || p.MaxQueueWait <= 0 {
		return 0, errors.New("store: SweepStaleInFlight needs positive UpstreamTimeout and MaxQueueWait")
	}
	nowMS := db.cfg.now().UnixMilli()
	dispatchCutoff := nowMS - p.UpstreamTimeout.Milliseconds() - p.Margin.Milliseconds()
	queueCutoff := nowMS - p.MaxQueueWait.Milliseconds() - p.Margin.Milliseconds()

	var n int64
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE idempotency
			   SET state = 'unknown'
			 WHERE state = 'in_flight'
			   AND (
			     (dispatch_started_at IS NOT NULL AND dispatch_started_at < ?)
			     OR
			     (dispatch_started_at IS NULL AND attempt_started_at < ?)
			   )`, dispatchCutoff, queueCutoff)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// IdempotencyPurgePeriod is the fixed idempotency cleanup interval.
const IdempotencyPurgePeriod = 15 * time.Minute

// clockJumpFactor is the clock-jump threshold: a gap of more than ten purge periods
// since the previous run is treated as a suspected forward clock jump. It is
// fixed as a MULTIPLE of the period rather than as an absolute
// number of minutes, so that it follows the period if the period ever becomes
// configurable.
const clockJumpFactor = 10

// purgeLastRunKey is the gateway_meta key holding the previous purge's own
// wall-clock time.
const purgeLastRunKey = "idempotency_purge_last_run_ms"

// PurgeExpiredIdempotency deletes rows past their TTL, guarded against a
// forward clock jump.
//
// expires_at is wall-clock: it has to be, because it must survive a restart.
// A clock that jumps FORWARD therefore expires records early; the purge deletes
// them, the next request with that key looks new, and it is dispatched a SECOND
// time — silently. A clock that jumps BACKWARD only makes records live longer,
// which is the safe direction.
//
// The mechanism: every run persists its own wall-clock time. If
// the next run finds that more than clockJumpFactor purge periods have elapsed
// since that stamp, it deletes nothing, returns ErrClockJumpSuspected, and
// advances the stamp anyway — otherwise a protection that fired once would stop
// the purge forever and the table would grow without bound. On a fresh database
// there is no stamp: no check is made, only the stamp is written.
//
// Read what this does and does not buy: it narrows the window, it does not
// close it. A jump that stays
// under the threshold is never seen; a jump that is seen only costs one run,
// because the rows still look expired on the next one. What is gained is that
// the double dispatch stops being SILENT. A long maintenance
// stop is indistinguishable from a jump and produces the same signal.
func (db *DB) PurgeExpiredIdempotency(ctx context.Context) (int64, error) {
	now := db.cfg.now()
	nowMS := now.UnixMilli()
	threshold := int64(clockJumpFactor) * IdempotencyPurgePeriod.Milliseconds()

	var (
		n         int64
		suspected bool
	)
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		var lastMS sql.NullInt64
		err := tx.QueryRowContext(ctx,
			`SELECT CAST(value AS INTEGER) FROM gateway_meta WHERE key = ?`, purgeLastRunKey).Scan(&lastMS)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// First run on this database: nothing to compare against.
		case err != nil:
			return err
		case lastMS.Valid && nowMS-lastMS.Int64 > threshold:
			suspected = true
		}

		if !suspected {
			res, err := tx.ExecContext(ctx, `DELETE FROM idempotency WHERE expires_at < ?`, nowMS)
			if err != nil {
				return err
			}
			if n, err = res.RowsAffected(); err != nil {
				return err
			}
		}

		// Stamped in both branches, and stamped with THIS run's wall clock even
		// when it moved backwards — each run persists its own time.
		// The cost is visible: a backward jump followed by a return to real
		// time trips the guard once on the next run.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO gateway_meta (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			purgeLastRunKey, strconv.FormatInt(nowMS, 10), nowMS)
		return err
	})
	if err != nil {
		return 0, err
	}
	if suspected {
		db.clockJumpSuspected.Add(1)
		return 0, ErrClockJumpSuspected
	}
	return n, nil
}

// ClockJumpSuspectedTotal is the process-lifetime count behind
// gateway_clock_jump_suspected_total. The store cannot emit metrics
// itself; the cleanup goroutine reads this.
func (db *DB) ClockJumpSuspectedTotal() int64 { return db.clockJumpSuspected.Load() }

// PurgeOldEvents applies gateway.retention_days to the trigger log.
//
// Four statements, one transaction, children before parents. The connection
// runs with PRAGMA foreign_keys = ON and trigger_event_id is a real foreign key onto
// trigger_events(event_id), so deleting the parent triggers first is a
// constraint violation; this order reaches the same end state.
//
// The fourth statement is trigger_rejected. Those rows are
// parentless, so the first three statements — which enumerate event types
// literally — all skip them. Without it, retention would never apply to the
// highest-volume event class in the table and the log would grow without
// bound, with nothing failing anywhere.
func (db *DB) PurgeOldEvents(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, errors.New("store: PurgeOldEvents needs a positive retention")
	}
	cutoff := db.cfg.now().Add(-retention).UnixMilli()
	var n int64
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		child, err := tx.ExecContext(ctx, `
			DELETE FROM trigger_events
			 WHERE event_type IN ('trigger_outcome', 'run_link', 'run_observed')
			   AND trigger_event_id IN (
			     SELECT event_id FROM trigger_events
			      WHERE event_type = 'trigger' AND ts < ?
			   )`, cutoff)
		if err != nil {
			return err
		}
		parent, err := tx.ExecContext(ctx, `
			DELETE FROM trigger_events WHERE event_type = 'trigger' AND ts < ?`, cutoff)
		if err != nil {
			return err
		}
		// Sweep any child left orphaned by an earlier partial delete.
		orphan, err := tx.ExecContext(ctx, `
			DELETE FROM trigger_events
			 WHERE event_type IN ('trigger_outcome', 'run_link', 'run_observed')
			   AND trigger_event_id IS NOT NULL
			   AND trigger_event_id NOT IN (
			     SELECT event_id FROM trigger_events WHERE event_type = 'trigger'
			   )`)
		if err != nil {
			return err
		}
		// trigger_rejected has neither parent nor children,
		// so it is deleted on its own ts and its position here is free.
		rejected, err := tx.ExecContext(ctx, `
			DELETE FROM trigger_events WHERE event_type = 'trigger_rejected' AND ts < ?`, cutoff)
		if err != nil {
			return err
		}
		for _, r := range []sql.Result{child, parent, orphan, rejected} {
			c, err := r.RowsAffected()
			if err != nil {
				return err
			}
			n += c
		}
		return nil
	})
	return n, err
}

// PurgeOldRuns applies inventory.retention_days to the github_runs cache.
func (db *DB) PurgeOldRuns(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, errors.New("store: PurgeOldRuns needs a positive retention")
	}
	cutoff := db.cfg.now().Add(-retention).UnixMilli()
	var n int64
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM github_runs WHERE created_at < ?`, cutoff)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}
