// Package cli holds the command implementations shared by both binaries:
// cmd/standpipe (the operator CLI) and cmd/standpiped (the gateway daemon).
//
// The split exists because a daemon and an operator CLI are run by different
// people in different situations, and an operator must not be able to start a
// listening server by mistyping a verb. Neither binary carries logic of its
// own — each is a wrapper around one exported entry point here (Operator,
// Daemon) — so a change to a command cannot land in one and not the other.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/store"
)

// newFlagSet builds a subcommand flag set that reports errors itself rather
// than printing Go's default usage twice.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// nowFunc is the CLI's clock. It exists as one symbol so that every command
// stamping a report (`inventory`'s generated_at, `status`'s uptime arithmetic)
// reads the same source, and so a test can pin it.
var nowFunc = time.Now

// openStore loads the configuration and opens the database the way the gateway
// does, including its schema check.
//
// The read-only commands (`inventory`, `export`, `trace`, `status`) run against
// a database that a LIVE gateway may be writing to. SQLite in WAL mode
// serves concurrent readers from a different process without blocking
// the writer, so none of them can stall a dispatch. `forget` is the exception
// and says so at its own call site.
//
// The schema check is not skipped for the CLI. A build that reads an older
// schema would answer `standpipe inventory` with a coverage number computed from
// columns that mean something else, and that is precisely the class of silent
// wrongness this project refuses: silent degradation, including silent success, is
// treated as the gravest failure mode.
func openStore(ctx context.Context, cfgPath string) (*config.Config, *store.DB, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(ctx, store.Config{
		Path:         cfg.Storage.SQLitePath,
		MinFreeBytes: cfg.Storage.MinFreeBytes,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("open storage: %w", err)
	}
	return cfg, db, nil
}

// writeJSONTo prints an indented JSON document. Every `--json` flag of `trace` and
// `status` goes through here, so the machine-readable output of one command cannot
// drift into a different shape from another's.
func writeJSONTo(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// parseTimestamp accepts RFC 3339, a bare date, or a unix seconds/millis
// value. `standpipe forget --before` and `standpipe export --since` take operator
// input, and refusing "2026-08-01" because it lacks a time of day teaches
// operators to avoid the command.
func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		// Milliseconds are what the event log stores; seconds are what an
		// operator gets from `date +%s`. Anything past the year 33658 in
		// seconds is a millisecond value.
		if n > 1e12 {
			return time.UnixMilli(n), nil
		}
		return time.Unix(n, 0), nil
	}
	return time.Time{}, fmt.Errorf("%q is not RFC 3339 (2026-08-01T00:00:00Z), a date (2026-08-01), or a unix timestamp", s)
}

// exitCode is an error that carries a specific process exit status.
//
// `standpipe status`'s exit code is a documented signal (it is "meant to be
// callable from a script") and `standpipe trace` does the same for a `trace` whose
// content_sha256 does not verify. Returning 1 for every failure would collapse
// "the system is unhealthy" into "the command could not run".
type exitCode struct {
	code int
	err  error
}

func (e exitCode) Error() string { return e.err.Error() }
func (e exitCode) Unwrap() error { return e.err }

// errExit builds an exitCode.
func errExit(code int, format string, args ...any) error {
	return exitCode{code: code, err: fmt.Errorf(format, args...)}
}

// silentExit ends the process with a status and no extra message, for commands
// that have already printed their own report.
type silentExit int

func (s silentExit) Error() string { return "" }

// humanDuration renders a span the way the trace and status examples do: "+342ms",
// "+4m 12s", "3h 12m".
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		if s := int(d.Seconds()) % 60; s != 0 {
			return fmt.Sprintf("%dm %ds", int(d.Minutes()), s)
		}
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh %dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// humanBytes renders a byte count for the `status` header line.
func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// confirm asks the operator a y/N question on stdin.
//
// A non-terminal stdin answers "no". `standpipe onboard` writes configuration,
// and a prompt that defaults to yes when piped would let a CI job add targets
// to the allowlist that nobody approved.
func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// reportExit maps an error to a process exit status. prog is the binary's own
// name, so a message an operator pastes into a bug report names the thing they
// actually ran.
//
// `trace` and `status` give specific exit codes a documented meaning, so "the command
// failed" and "the system is unhealthy" must not collapse into the same
// status. A silentExit has already printed its own report; an exitCode carries
// both a message and a status; anything else is an ordinary failure.
func reportExit(prog string, err error) int {
	var silent silentExit
	if errors.As(err, &silent) {
		return int(silent)
	}
	var coded exitCode
	if errors.As(err, &coded) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, coded)
		return coded.code
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
	return 1
}
