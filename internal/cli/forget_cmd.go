package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// runForgetCmd implements `standpipe forget --before <timestamp>`.
//
// It NULLs the three redactable fields — `source_ip`, `reason_text`,
// `user_agent` — and nothing else. Two properties make that safe, and both
// live in internal/store rather than here:
//
//  1. The content_sha256 hash scope EXCLUDES the redactable fields, so
//     redaction does not invalidate a single digest. The event log stays
//     verifiable after a GDPR erasure; that is what the exclusion is for.
//  2. Redaction covers `trigger` AND `trigger_rejected`.
//     Both carry caller identity. An implementation that redacted only
//     `trigger` would leave the IP addresses of REJECTED requests behind — a
//     silent compliance hole, and exactly the trap of a query that hard-codes
//     a literal list of event types and misses one.
//
// The append-only rule is not broken by this: no row is deleted and no
// row's meaning changes. Three columns become NULL, which is the documented
// erasure mechanism.
func runForgetCmd(args []string) error {
	fs := newFlagSet("forget")
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	before := fs.String("before", "", "redact events older than this (RFC 3339, a date, or unix seconds)")
	dryRun := fs.Bool("dry-run", false, "report how many rows WOULD be redacted and change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *before == "" {
		return errors.New("forget: --before is required")
	}
	cutoff, err := parseTimestamp(*before)
	if err != nil {
		return fmt.Errorf("forget --before: %w", err)
	}

	ctx := context.Background()
	_, db, err := openStore(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if *dryRun {
		n, err := db.ForgetPlan(ctx, cutoff)
		if err != nil {
			return err
		}
		fmt.Printf("dry-run: %d row(s) before %s carry at least one redactable field\n", n, cutoff.UTC().Format("2006-01-02T15:04:05Z"))
		fmt.Printf("         fields: caller_source_ip, caller_user_agent, reason_text\n")
		fmt.Printf("         scope:  trigger AND trigger_rejected rows\n")
		fmt.Printf("         nothing was changed\n")
		return nil
	}

	// Unlike the other CLI commands this one WRITES, and the gateway's single-writer
	// model assumes one process. Running it against a live gateway's database
	// puts a second writer on the file: SQLite serialises them and
	// busy_timeout covers the wait, so it is safe, but the redaction
	// UPDATE holds the write lock in front of dispatches for its duration.
	// Say so rather than let an operator discover it as a latency spike.
	fmt.Fprintf(os.Stderr, "standpipe forget: taking the write lock; if the gateway is running, dispatches queue behind this\n")

	n, err := db.Forget(ctx, cutoff)
	if err != nil {
		return err
	}
	fmt.Printf("redacted %d row(s) written before %s\n", n, cutoff.UTC().Format("2006-01-02T15:04:05Z"))
	fmt.Printf("  content_sha256 is UNAFFECTED: the redactable fields are outside the hash scope,\n")
	fmt.Printf("  so every redacted event still verifies against its recorded digest.\n")
	return nil
}
