package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Pragma defaults.
const (
	defaultBusyTimeout       = 5 * time.Second
	defaultWALAutocheckpoint = 1000
	defaultReadPoolSize      = 4
)

// Config configures a DB. Only Path is required.
type Config struct {
	// Path is the SQLite file path (storage.sqlite_path).
	Path string

	// ReadPoolSize caps the read pool. Zero means 4.
	ReadPoolSize int

	// BusyTimeout is PRAGMA busy_timeout. Zero means 5s.
	BusyTimeout time.Duration

	// WALAutocheckpoint is PRAGMA wal_autocheckpoint. Zero means 1000.
	WALAutocheckpoint int

	// MinFreeBytes is storage.min_free_bytes. Zero disables the check.
	MinFreeBytes uint64

	// Now supplies wall-clock time. Zero value means time.Now.
	//
	// Everything this package persists (expires_at, ts, attempt_started_at) is
	// wall-clock by necessity: a monotonic reading is meaningless across a
	// restart. Callers that need elapsed-time decisions inside one process
	// should use a monotonic clock themselves. A forward clock jump expires
	// idempotency records early; PurgeExpiredIdempotency carries the guard.
	Now func() time.Time

	// NewEventID mints event ids for events this package originates
	// (run_link, run_observed). Zero value means NewEventID.
	NewEventID func() string
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Config) newEventID() string {
	if c.NewEventID != nil {
		return c.NewEventID()
	}
	return NewEventID()
}

// DB is the SQLite-backed Store. It holds two pools: a single-connection
// writer and a small read pool. Every write goes through writeTx,
// which opens the transaction with BEGIN IMMEDIATE.
type DB struct {
	cfg    Config
	writer *sql.DB
	reader *sql.DB
	path   string

	// clockJumpSuspected backs gateway_clock_jump_suspected_total.
	clockJumpSuspected atomic.Int64
}

var _ Store = (*DB)(nil)

// dsn builds a modernc.org/sqlite DSN. The pragmas are attached as
// _pragma query parameters because they are per-connection settings and the
// read pool opens more than one connection; setting them once after Open would
// leave later connections unconfigured.
func (c *Config) dsn(readOnly bool) string {
	busy := c.BusyTimeout
	if busy <= 0 {
		busy = defaultBusyTimeout
	}
	autockpt := c.WALAutocheckpoint
	if autockpt == 0 {
		autockpt = defaultWALAutocheckpoint
	}

	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.FormatInt(busy.Milliseconds(), 10)+")")
	q.Add("_pragma", "foreign_keys(ON)")
	if readOnly {
		// mode=ro is what makes the single-writer rule structural rather
		// than conventional: a stray write on a read-pool connection fails at
		// the driver instead of quietly taking the write lock in front of a
		// dispatch.
		//
		// journal_mode, synchronous and wal_autocheckpoint are omitted here
		// because they govern writing. They do NOT fail on a read-only
		// connection — verified: modernc.org/sqlite applies them and SQLite
		// quietly leaves the setting alone — which is precisely why sending
		// them would be misleading. busy_timeout is what matters on a read
		// connection: it bounds the wait while the WAL is checkpointed.
		//
		// mode=ro cannot CREATE the file (SQLITE_CANTOPEN). The writer above
		// has already created it, so this ordering is load-bearing.
		q.Set("mode", "ro")
		return "file:" + c.Path + "?" + q.Encode()
	}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "wal_autocheckpoint("+strconv.Itoa(autockpt)+")")
	// _txlock is deliberately not set. The reservation protocol issues
	// BEGIN IMMEDIATE explicitly on a pinned connection so that the lock mode
	// is visible in this package rather than in a connection string.
	return "file:" + c.Path + "?" + q.Encode()
}

// Open opens the database, applies the pragmas and verifies that the schema is
// at the version this build expects. It returns ErrSchemaOutdated when
// a migration is pending: the gateway must refuse to start and wait for an
// explicit `standpipe migrate`.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	db, err := OpenUnchecked(ctx, cfg)
	if err != nil {
		return nil, err
	}
	have, err := db.SchemaVersion(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	want := LatestSchemaVersion()
	switch {
	case have < want:
		db.Close()
		return nil, fmt.Errorf("%w: database is at schema version %d, this build expects %d; run `standpipe migrate`", ErrSchemaOutdated, have, want)
	case have > want:
		db.Close()
		return nil, fmt.Errorf("%w: database is at schema version %d, this build only knows %d", ErrSchemaNewer, have, want)
	}
	// The version number only says WHICH migrations ran; the checksums say
	// whether they are still the migrations this build embeds. Checking
	// only user_version here left the guard unreachable at startup: an edited
	// migration file produced a matching version and a silently different
	// schema.
	if err := db.VerifyAppliedChecksums(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenUnchecked opens the database without the schema version check. It exists
// for `standpipe migrate`, which by definition runs against a database whose
// version does not match yet.
func OpenUnchecked(ctx context.Context, cfg Config) (*DB, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("store: Config.Path is empty")
	}
	abs, err := filepath.Abs(cfg.Path)
	if err != nil {
		return nil, err
	}
	cfg.Path = abs

	writer, err := sql.Open("sqlite", cfg.dsn(false))
	if err != nil {
		return nil, err
	}
	// Single writer. All writes are serialised by the pool itself, so
	// BEGIN IMMEDIATE never has to contend with another connection of ours.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)

	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		return nil, fmt.Errorf("store: opening %s: %w", cfg.Path, err)
	}

	pool := cfg.ReadPoolSize
	if pool <= 0 {
		pool = defaultReadPoolSize
	}
	// Read-only. The writer above has already created and configured
	// the file, so a mode=ro handle can attach to it.
	reader, err := sql.Open("sqlite", cfg.dsn(true))
	if err != nil {
		writer.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(pool)
	reader.SetMaxIdleConns(pool)
	reader.SetConnMaxLifetime(0)
	if err := reader.PingContext(ctx); err != nil {
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("store: opening %s read-only: %w", cfg.Path, err)
	}

	return &DB{cfg: cfg, writer: writer, reader: reader, path: cfg.Path}, nil
}

// Close closes both pools.
func (db *DB) Close() error {
	var errs []error
	if db.reader != nil {
		errs = append(errs, db.reader.Close())
	}
	if db.writer != nil {
		errs = append(errs, db.writer.Close())
	}
	return errors.Join(errs...)
}

// Path is the absolute path of the database file.
func (db *DB) Path() string { return db.path }

// Reader exposes the read pool for read-only queries owned by this package.
// It is unexported behaviour by convention: no other package should build SQL,
// so this returns nothing that can be used to write.
func (db *DB) readDB() *sql.DB { return db.reader }

// writeTx runs fn inside BEGIN IMMEDIATE on the single writer connection.
// The connection is pinned for the whole transaction, so no other
// statement can interleave.
//
// BEGIN IMMEDIATE is issued by hand rather than through sql.Tx because
// database/sql has no way to express the lock mode, and the reservation
// protocol depends on taking the write lock at BEGIN rather than at the first
// write.
func (db *DB) writeTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Conn) error) (err error) {
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: BEGIN IMMEDIATE: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Roll back even when ctx is already cancelled, otherwise the pinned
		// connection returns to the pool inside an open transaction and the
		// next writer inherits it.
		if _, rbErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rbErr != nil && err == nil {
			err = fmt.Errorf("store: ROLLBACK: %w", rbErr)
		}
	}()

	if err := fn(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: COMMIT: %w", err)
	}
	committed = true
	return nil
}

// HealthCheck runs PRAGMA quick_check. It writes nothing and runs on
// the read pool, so it never contends with a dispatch on the single writer.
func (db *DB) HealthCheck(ctx context.Context) error {
	var result string
	if err := db.reader.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&result); err != nil {
		return fmt.Errorf("store: quick_check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("store: quick_check reported %q", result)
	}
	return nil
}

// BackupTo takes a consistent snapshot with VACUUM INTO. The
// destination must not exist. The snapshot is a single self-contained file:
// unlike a file copy it needs no accompanying -wal file to be restorable.
//
// It runs on the writer pool so that it serialises against ordinary writes.
func (db *DB) BackupTo(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// VACUUM INTO takes no bound parameters, so the path is quoted as a SQL
	// string literal.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(abs, "'", "''") + "'"
	if _, err := db.writer.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("store: VACUUM INTO %s: %w", abs, err)
	}
	return nil
}

// CheckFreeSpace reports ErrStorageDegraded when the filesystem holding the
// database has less than Config.MinFreeBytes available. The gateway
// turns that into 503 storage_degraded. It is a proactive check: an actual
// out-of-space write failure surfaces separately, see IsDiskFull.
func (db *DB) CheckFreeSpace() error {
	if db.cfg.MinFreeBytes == 0 {
		return nil
	}
	free, err := FreeBytes(filepath.Dir(db.path))
	if err != nil {
		return err
	}
	if free < db.cfg.MinFreeBytes {
		return fmt.Errorf("%w: %d bytes free, %d required", ErrStorageDegraded, free, db.cfg.MinFreeBytes)
	}
	return nil
}
