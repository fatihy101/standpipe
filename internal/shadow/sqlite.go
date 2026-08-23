package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Pragma defaults. Same discipline as gateway.db's; the values are repeated
// here rather than imported so that a change to the gateway's tuning does not
// silently retune the ledger.
const (
	defaultBusyTimeout       = 5 * time.Second
	defaultWALAutocheckpoint = 1000
	defaultReadPoolSize      = 2
)

// Config configures the shadow ledger. Only Path is required.
type Config struct {
	// Path is storage.shadow_sqlite_path. It must NOT be gateway.db.
	Path string

	// ReadPoolSize caps the read pool. Zero means 2 — the audit queries
	// are the only readers and they are operator-driven, not per-request.
	ReadPoolSize int

	// BusyTimeout is PRAGMA busy_timeout. Zero means 5s.
	BusyTimeout time.Duration

	// WALAutocheckpoint is PRAGMA wal_autocheckpoint. Zero means 1000.
	WALAutocheckpoint int

	// Now supplies wall-clock time for applied_at stamps. Zero means time.Now.
	//
	// Everything this package persists is wall-clock by necessity. That has a
	// consequence the day axis cannot escape: `day` is a STRING drawn in a
	// configured timezone by the caller, so if the clock or the zone moves, two
	// day rows can describe overlapping wall-clock intervals and the ledger
	// cannot tell. The timezone key is still an open question.
	Now func() time.Time
}

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// DB is the shadow ledger. One writer connection, a small read pool. Every
// write goes through writeTx, which opens with BEGIN IMMEDIATE.
type DB struct {
	cfg    Config
	writer *sql.DB
	reader *sql.DB
	path   string
}

// dsn builds a modernc.org/sqlite DSN. The pragmas are attached as _pragma
// query parameters because they are PER CONNECTION and the read pool opens
// more than one; setting them once after Open would leave later connections
// unconfigured. The driver applies busy_timeout first regardless of order.
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
	// foreign_keys(ON) is load-bearing twice over: it is what makes an
	// unrecorded drill_day reject a run, and it is what makes the retention
	// order in Purge* mandatory rather than tidy.
	q.Add("_pragma", "foreign_keys(ON)")
	if readOnly {
		// mode=ro makes the single-writer rule structural. A stray write on a
		// read-pool connection fails at the driver instead of quietly taking
		// the write lock in front of a record write.
		//
		// The write-side pragmas are omitted because they govern writing. They
		// do not FAIL on a read-only connection — SQLite quietly leaves the
		// setting alone — which is exactly why sending them would be
		// misleading. mode=ro also cannot CREATE the file, so the writer must
		// be opened first; that ordering is load-bearing.
		q.Set("mode", "ro")
		return "file:" + c.Path + "?" + q.Encode()
	}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "wal_autocheckpoint("+strconv.Itoa(autockpt)+")")
	// _txlock is deliberately not set: database/sql cannot express a lock mode,
	// and hiding BEGIN IMMEDIATE in a URL puts the most important line of the
	// write path somewhere nobody reads. writeTx issues it by hand.
	return "file:" + c.Path + "?" + q.Encode()
}

// Open opens shadow.db and verifies the schema is at the version this build
// expects. It returns ErrSchemaOutdated when a migration is pending.
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
		return nil, fmt.Errorf("%w: shadow.db is at schema version %d, this build expects %d", ErrSchemaOutdated, have, want)
	case have > want:
		db.Close()
		return nil, fmt.Errorf("%w: shadow.db is at schema version %d, this build only knows %d", ErrSchemaNewer, have, want)
	}
	// The version number only says WHICH migrations ran; the checksums say
	// whether they are still the migrations this build embeds. Checking only
	// user_version leaves the guard unreachable: an edited migration file
	// produces a matching version and a silently different schema.
	if err := db.VerifyAppliedChecksums(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenUnchecked opens without the version check, for the migrate path.
func OpenUnchecked(ctx context.Context, cfg Config) (*DB, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("shadow: Config.Path is empty")
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
	// Single writer. All writes are serialised by the pool, so BEGIN IMMEDIATE
	// never contends with another connection of ours.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)
	writer.SetConnMaxIdleTime(0)
	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		return nil, fmt.Errorf("shadow: opening %s: %w", cfg.Path, err)
	}

	pool := cfg.ReadPoolSize
	if pool <= 0 {
		pool = defaultReadPoolSize
	}
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
		return nil, fmt.Errorf("shadow: opening %s read-only: %w", cfg.Path, err)
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

// Path is the absolute path of the ledger file.
func (db *DB) Path() string { return db.path }

// writeTx runs fn inside BEGIN IMMEDIATE on the single writer connection. The
// connection is pinned for the whole transaction, so no other statement can
// interleave.
//
// BEGIN IMMEDIATE is issued by hand rather than through sql.Tx because
// database/sql cannot express the lock mode.
func (db *DB) writeTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Conn) error) (err error) {
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("shadow: BEGIN IMMEDIATE: %w", err)
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
			err = fmt.Errorf("shadow: ROLLBACK: %w", rbErr)
		}
	}()

	if err := fn(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("shadow: COMMIT: %w", err)
	}
	committed = true
	return nil
}

// HealthCheck runs PRAGMA quick_check on the read pool.
func (db *DB) HealthCheck(ctx context.Context) error {
	var result string
	if err := db.reader.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&result); err != nil {
		return fmt.Errorf("shadow: quick_check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("shadow: quick_check reported %q", result)
	}
	return nil
}

// BackupTo takes a consistent snapshot with VACUUM INTO. The destination must
// not exist. It runs on the writer pool so that it serialises against writes.
func (db *DB) BackupTo(ctx context.Context, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// VACUUM INTO takes no bound parameters, so the path is quoted as a SQL
	// string literal.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(abs, "'", "''") + "'"
	if _, err := db.writer.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("shadow: VACUUM INTO %s: %w", abs, err)
	}
	return nil
}
