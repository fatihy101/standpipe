package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"time"
)

// runExportCmd implements `standpipe export`.
//
// It is split from `standpipe inventory` on purpose and the split is the whole
// point of the change: `inventory` produces a SUMMARY, an archive dump is a
// different operation, and one name for both misled operators. The old
// `--export-events` flag is refused by name in inventory_cmd.go rather than
// ignored.
//
// The scope is EVERY event type — trigger, trigger_outcome, trigger_rejected,
// run_link, run_observed. store.ExportEvents refuses to write an unknown type
// with envelope fields only, because a line whose content_sha256 cannot be
// recomputed quietly empties the log's testability claim. This file
// therefore adds no filtering of its own: a `--type` flag would recreate the
// same hole with a flag instead of a bug.
func runExportCmd(args []string) error {
	fs := newFlagSet("export")
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	since := fs.String("since", "", "only events at or after this time (RFC 3339)")
	out := fs.String("out", "", "write here instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var sinceTS time.Time
	if *since != "" {
		t, err := parseTimestamp(*since)
		if err != nil {
			return fmt.Errorf("export --since: %w", err)
		}
		sinceTS = t
	}

	ctx := context.Background()
	_, db, err := openStore(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	dst := os.Stdout
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		dst = f
	}
	// Buffered: the export is one write syscall per event otherwise, and this
	// file is meant to be run over a 90-day retention window.
	bw := bufio.NewWriterSize(dst, 128*1024)

	n, err := db.ExportEvents(ctx, sinceTS, bw)
	if flushErr := bw.Flush(); err == nil {
		err = flushErr
	}
	if err != nil {
		// The count is reported even on failure. A partial export that looks
		// like a whole one is the archive equivalent of a silent degradation:
		// the operator must know the file is truncated.
		fmt.Fprintf(os.Stderr, "standpipe export: wrote %d event(s) before failing\n", n)
		return err
	}
	// The count goes to stderr so `standpipe export > events.jsonl`
	// produces a file of nothing but events.
	fmt.Fprintf(os.Stderr, "exported %d event(s)\n", n)
	return nil
}
