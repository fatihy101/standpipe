package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/fatihy101/standpipe/internal/httpapi"
	"github.com/fatihy101/standpipe/internal/store"
)

// runInventoryCmd implements `standpipe inventory`.
//
// This is the command named in Phase 0's exit condition:
// "`standpipe inventory` shows a bootstrapped list plus the coverage gap". Until
// it existed, the gateway could record everything and show nobody anything,
// and the project's central claim — we record everything and never silently
// degrade — was untestable from outside.
//
// It reads ONLY local SQLite. It makes no GitHub call, so it answers during
// the outage the product exists for, and it needs no running gateway: the
// bootstrapped surface and the run history are both persistent.
//
// The body is built by httpapi.BuildInventory, the same function behind the
// admin /inventory endpoint. Two implementations would eventually
// disagree about the coverage gap, and the gap is the number the phase is
// judged on.
func runInventoryCmd(args []string) error {
	fs := newFlagSet("inventory")
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	routingPath := fs.String("routing", "", "also validate this routing.json")
	human := fs.Bool("human", false, "print a summary table instead of JSON")
	// `standpipe inventory` no longer recognises --export-events. The flag
	// is DECLARED so that giving it produces an error naming the replacement,
	// rather than flag's "flag provided but not defined" — an operator with a
	// script from the previous naming needs to be pointed somewhere.
	exportEvents := fs.Bool("export-events", false, "removed — see `standpipe export`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *exportEvents {
		return errors.New("inventory: --export-events was removed in favour of `standpipe export`.\n" +
			"        `inventory` produces a SUMMARY; an archive dump is a different operation and the shared\n" +
			"        name misled operators. Run: standpipe export [--since <RFC3339>] > events.jsonl")
	}

	ctx := context.Background()
	cfg, db, err := openStore(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	report, err := httpapi.BuildInventory(ctx, db, cfg.Inventory.HistoryDays, nowFunc())
	if err != nil {
		return err
	}

	// routing.json is validated HERE, in the CLI, and never at runtime.
	if *routingPath != "" {
		report.Routing = validateRoutingForInventory(*routingPath)
	}

	if *human {
		printInventoryHuman(os.Stdout, report, cfg.Inventory.HistoryDays)
	} else if err := writeJSONTo(os.Stdout, report); err != nil {
		return err
	}

	// A structurally invalid routing.json is a failure of this command, not a
	// footnote: the direct-call runbook has the caller read that file to pick a
	// backend during an outage, so a broken one is discovered at the worst possible moment.
	if report.Routing != nil && !report.Routing.Valid {
		return silentExit(1)
	}
	return nil
}

func validateRoutingForInventory(path string) *httpapi.RoutingStatus {
	st := &httpapi.RoutingStatus{Path: path}
	f, raw, err := LoadRouting(path)
	if err != nil {
		st.Errors = []string{err.Error()}
		return st
	}
	st.Errors = ValidateRouting(f, raw)
	st.Valid = len(st.Errors) == 0
	return st
}

// printInventoryHuman is the operator-facing rendering. The JSON is the
// specified form, and what `jq` consumes; this exists because reading
// a coverage split out of unindented JSON in a terminal is how a structural
// gap gets mistaken for a bug.
func printInventoryHuman(w *os.File, r *httpapi.InventoryReport, historyDays int) {
	fmt.Fprintf(w, "SURFACE (%d workflows in the persistent cache)\n", len(r.Surface))
	rows := append([]httpapi.SurfaceEntry(nil), r.Surface...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Repo != rows[j].Repo {
			return rows[i].Repo < rows[j].Repo
		}
		return rows[i].Workflow < rows[j].Workflow
	})
	if len(rows) == 0 {
		fmt.Fprintf(w, "  (empty — bootstrap has not completed, or no allowed_targets are configured)\n")
	}
	for _, s := range rows {
		ready := "correlation_ready: no  (tier 2 not armed; tier 1 still yields `exact`)"
		if s.CorrelationReady {
			ready = "correlation_ready: yes"
		}
		fmt.Fprintf(w, "  %-24s %-20s [%s]  %s\n", s.Repo, s.Workflow, strings.Join(s.Triggers, ","), ready)
	}

	fmt.Fprintf(w, "\nOBSERVED (%d rows, %dd window)\n", len(r.Observed), historyDays)
	for _, o := range r.Observed {
		actor, caller := "-", "-"
		if o.GitHubActor != nil {
			actor = *o.GitHubActor
		}
		if o.Caller != nil {
			caller = *o.Caller
		}
		fmt.Fprintf(w, "  %-9s %-24s %-20s actor=%-18s caller=%-14s %d/wk\n",
			o.Coverage, o.Repo, o.Workflow, actor, caller, o.WeeklyCount)
	}

	fmt.Fprintf(w, "\nCOVERAGE (%dd, over %d runs)\n", historyDays, r.TotalRuns)
	fmt.Fprintf(w, "  covered  %d\n", r.CoveredCount)
	fmt.Fprintf(w, "  unknown  %d  (%.1f%%)  — went through the gateway, run_id not settled\n",
		r.CoverageUnknownCnt, r.CoverageUnknownPct)
	fmt.Fprintf(w, "  gap      %d  (%.1f%%)  — never went through the gateway\n",
		r.CoverageGapCount, r.CoverageGapPct)
	fmt.Fprintf(w, "    migratable %d  — known caller, workflow_dispatch; this is Phase 0's success measure\n",
		r.CoverageGapByClass[store.GapMigratable])
	fmt.Fprintf(w, "    structural %d  — UI runs, repository_dispatch, accepted direct calls; never converges to zero\n",
		r.CoverageGapByClass[store.GapStructural])
	fmt.Fprintf(w, "  gap_pct and unknown_pct are separate gauges and are never summed.\n")

	if r.Routing != nil {
		fmt.Fprintf(w, "\nROUTING (%s)\n", r.Routing.Path)
		if r.Routing.Valid {
			fmt.Fprintf(w, "  valid\n")
		} else {
			for _, e := range r.Routing.Errors {
				fmt.Fprintf(w, "  INVALID: %s\n", e)
			}
		}
	}
}
