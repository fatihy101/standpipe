package store

import (
	"errors"

	"modernc.org/sqlite"
)

var (
	// ErrSchemaOutdated means a migration is pending. The gateway refuses to
	// start; the operator runs `standpipe migrate`.
	ErrSchemaOutdated = errors.New("store: schema outdated")

	// ErrSchemaNewer means the database was written by a newer build. There is
	// no downgrade path — restore a snapshot.
	ErrSchemaNewer = errors.New("store: schema newer than this build")

	// ErrSchemaChecksum means schema_version does not agree with the embedded
	// migration source: the file was edited after it was applied, so the
	// database contains something other than what this build believes.
	// Both Open and Migrate refuse.
	ErrSchemaChecksum = errors.New("store: migration checksum mismatch")

	// ErrStorageDegraded means free disk space is under storage.min_free_bytes.
	// The gateway answers 503 storage_degraded + Retry-After.
	ErrStorageDegraded = errors.New("store: free space below threshold")

	// ErrNotOwner means a conditional UPDATE matched no row: the idempotency
	// record was taken over by another attempt between reservation and
	// completion. changes() == 0 in the reservation protocol's completion UPDATE and in the sweep's
	// retry CAS. The caller must not treat its own result as authoritative.
	ErrNotOwner = errors.New("store: idempotency record no longer owned by this attempt")

	// ErrNotFound means a lookup matched no row.
	ErrNotFound = errors.New("store: not found")

	// ErrWeakerResolution means an AppendRunLink was skipped because the
	// existing active run_link is at least as reliable. Not a failure.
	ErrWeakerResolution = errors.New("store: existing run_link resolution is not weaker")

	// ErrClockJumpSuspected means PurgeExpiredIdempotency refused to delete
	// anything because far more wall-clock time has passed since the previous
	// run than the purge period allows. Nothing was deleted; the
	// stamp was still advanced, so the next run proceeds normally.
	//
	// It is returned rather than logged internally because the store has no
	// logger: the caller emits gateway_clock_jump_suspected_total and a warn
	// line. A false positive is expected after a long maintenance stop —
	// a long stop is indistinguishable from a jump, which is why this must not
	// drive an alert on its own.
	ErrClockJumpSuspected = errors.New("store: idempotency purge skipped, forward clock jump suspected")
)

// SQLite primary result codes used here. Kept as constants rather than
// importing modernc.org/sqlite/lib, which pulls the whole translated C header
// into the build for four integers.
const (
	sqliteBusy       = 5
	sqliteIOErr      = 10
	sqliteFull       = 13
	sqliteConstraint = 19
)

// primaryResultCode extracts the primary SQLite result code from err, or -1
// when err did not come from the driver. modernc.org/sqlite returns
// *sqlite.Error whose Code() carries the extended code; the low byte is the
// primary code.
func primaryResultCode(err error) int {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return -1
	}
	return serr.Code() & 0xff
}

// IsDiskFull reports whether err is SQLite running out of space. SQLITE_FULL
// is the direct signal; SQLITE_IOERR covers the case where the filesystem
// refuses the write before SQLite notices, which is what a full tmpfs or a
// quota typically produces.
//
// This is the reactive path. The proactive one is DB.CheckFreeSpace; a write that fails anyway is a gateway 500
// internal_error, not a 503, because durable storage was expected to
// work and did not.
func IsDiskFull(err error) bool {
	code := primaryResultCode(err)
	return code == sqliteFull || code == sqliteIOErr
}

// IsBusy reports whether err is SQLITE_BUSY, i.e. busy_timeout elapsed while
// waiting for a lock.
func IsBusy(err error) bool { return primaryResultCode(err) == sqliteBusy }

// IsConstraint reports whether err is a constraint violation. The one this
// package can hit in normal operation is the partial UNIQUE index guarding a
// single active run_link per trigger.
func IsConstraint(err error) bool { return primaryResultCode(err) == sqliteConstraint }
