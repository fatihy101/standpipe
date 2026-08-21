package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// runTraceCmd implements `standpipe trace <event-id>`.
//
// It shows the WHOLE LIFE of one trigger on one screen: what arrived, what was
// forwarded, which run it correlated to, with which resolution and from which
// tier. The acceptance procedure is written against this command — it replaces three
// grep/jq pipelines over the JSONL export, and with them the operator's need
// to join the event log by hand.
//
// Two rules shape every line below:
//
//  1. It adds no capability. No GitHub call is made; everything comes from
//     local SQLite. `trace` can therefore answer during the outage that
//     produced the trigger being traced.
//  2. Every missing piece is printed as EXPLICITLY missing. An absent run_link
//     prints "unresolved" with the horizon; an absent run_observed prints "no
//     conclusion yet". A blank line where a fact should be is the silent
//     degradation this project puts first among failure modes.
func runTraceCmd(args []string) error {
	fs := newFlagSet("trace")
	cfgPath := fs.String("config", "standpipe.yaml", "path to standpipe.yaml")
	asJSON := fs.Bool("json", false, "machine-readable output")
	history := fs.Bool("history", false, "also show superseded run_links")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("trace: expected exactly one event id\n" +
			"        usage: standpipe trace <event-id> [--json] [--history]\n" +
			"        the id may be the trigger's, or that of any run_link / trigger_outcome /\n" +
			"        run_observed child, or a trigger_rejected id")
	}
	wanted := fs.Arg(0)

	ctx := context.Background()
	_, db, err := openStore(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	tr, err := buildTrace(ctx, db, wanted)
	if err != nil {
		return err
	}

	if *asJSON {
		if err := writeJSONTo(os.Stdout, tr.json(*history)); err != nil {
			return err
		}
	} else {
		tr.print(os.Stdout, *history)
	}

	// The exit code, which is NOT "any verdict that is not ok".
	//
	//   MISMATCH                       -> non-zero. The row changed.
	//   UNVERIFIABLE row_malformed     -> non-zero. A row that got past a
	//                                    write-time CHECK is a writer fault and
	//                                    is handled like a mismatch.
	//   UNVERIFIABLE unknown_schema_   -> DOES NOT AFFECT THE EXIT CODE. An
	//   version                          archive may carry several
	//                                    schema versions side by side, so this
	//                                    is the expected state, and a check that
	//                                    burns red permanently is a check nobody
	//                                    reads.
	//   row_missing                    -> not an integrity verdict at all; it is
	//                                    trace's own "no such event" path.
	//
	// The verdicts are printed above either way — the exit code grades them, it
	// never replaces them.
	if bad := tr.integrityFailures(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "\nintegrity: %s\n", strings.Join(bad, "\nintegrity: "))
		return silentExit(1)
	}
	return nil
}

// integrityFailures lists the events whose verdict must turn the
// exit code non-zero, each with the sentence that explains it. An empty slice
// means every event either verified or could not be spoken for by this build
// for a reason the integrity rules call expected.
func (tr *traceResult) integrityFailures() []string {
	var out []string
	for _, e := range tr.verifiedEvents() {
		res, ok := tr.Integrity[e.eventID()]
		if !ok {
			continue
		}
		switch {
		case res.Verdict == store.IntegrityMismatch:
			out = append(out, fmt.Sprintf("%s MISMATCH — %s", e.eventID(), res.Detail))
		case res.Verdict == store.IntegrityUnverifiable && integrityReasonIsFault(res.Reason):
			out = append(out, fmt.Sprintf("%s UNVERIFIABLE (%s) — %s", e.eventID(), res.Reason, res.Detail))
		}
	}
	return out
}

// integrityReasonIsFault splits the unverifiable reasons into the ones that
// mean "somebody wrote a row that should not exist" and the ones that mean
// "this build is not the build that can answer".
//
// The specification names three reasons; internal/store returns five. The two
// it adds sort the same way as their named neighbours and are treated so here:
//
//   - unknown_event_type sorts with unknown_schema_version — a database written
//     by a build that knows more event types than this one. Expected in a mixed
//     archive, not a fault.
//   - stored_hash_missing sorts with row_malformed — content_sha256 is NOT NULL
//     in the schema, so an empty one is a row that did not come through this package.
func integrityReasonIsFault(r store.IntegrityReason) bool {
	switch r {
	case store.IntegrityReasonRowMalformed, store.IntegrityReasonStoredHashMissing:
		return true
	default:
		// unknown_schema_version, unknown_event_type, row_missing.
		return false
	}
}

// traceResult is one trigger's whole life.
type traceResult struct {
	// Rejected is set instead of Trigger when the id named a
	// trigger_rejected. That event has no parent and no children, so the
	// output is a single REJECTED block.
	Rejected eventObj

	Trigger    eventObj
	Outcome    eventObj
	ActiveLink eventObj
	Superseded []eventObj
	Observed   eventObj

	Coverage string

	// Integrity holds one integrity verdict per event in this trace, keyed by
	// event_id. trace requires content_sha256 to be RECOMPUTED for every event
	// and reported with one of three verdicts, so this is a map and not a
	// single field: two events of the same trigger can legitimately disagree
	// (a run_link written by a newer build next to a trigger written by this
	// one), and collapsing them would have to pick a winner.
	Integrity map[string]store.IntegrityResult

	// OrphanedParent is set when a child event named a trigger that is no
	// longer in the log — retention purged the parent, or a half-finished
	// purge left the child behind (the retention purge's orphan step removes it).
	OrphanedParent string
}

// buildTrace resolves an id to its trigger and gathers that trigger's children.
func buildTrace(ctx context.Context, db *store.DB, id string) (*traceResult, error) {
	raw, err := db.EventByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("trace: no event with id %q.\n"+
			"        The retention window is gateway.retention_days; an event older than that\n"+
			"        has been purged and its absence here is not a failure", id)
	}
	if err != nil {
		return nil, err
	}
	ev := eventObj(raw)

	tr := &traceResult{}

	switch ev.eventType() {
	case string(store.EventTriggerRejected):
		tr.Rejected = ev
		return tr, tr.verify(ctx, db)
	case string(store.EventTrigger):
		tr.Trigger = ev
	default:
		// Given a child's id, find the parent trigger first and print
		// ITS trace. An operator holding a run_link id from a log line should
		// not have to work backwards by hand.
		parent := ev.parent()
		if parent == "" {
			return nil, fmt.Errorf("trace: event %s is a %s with no trigger_event_id; it cannot be traced to a trigger",
				id, ev.eventType())
		}
		praw, err := db.EventByID(ctx, parent)
		if errors.Is(err, store.ErrNotFound) {
			tr.OrphanedParent = parent
			tr.Trigger = ev
			return tr, tr.verify(ctx, db)
		}
		if err != nil {
			return nil, err
		}
		tr.Trigger = eventObj(praw)
	}

	rootID := tr.Trigger.eventID()
	rootTS := tr.Trigger.ts()

	// Children are collected by scanning forward from the trigger's own
	// timestamp: no child can precede its parent. See scanEvents for why this
	// is a scan and what it would take to make it a seek.
	err = scanEvents(ctx, db, rootTS, func(child eventObj) error {
		if child.parent() != rootID {
			return nil
		}
		switch child.eventType() {
		case string(store.EventTriggerOutcome):
			// The gateway writes one outcome per trigger. If a second ever appears,
			// the later one is what the caller was answered with.
			if tr.Outcome == nil || child.ts().After(tr.Outcome.ts()) {
				tr.Outcome = child
			}
		case string(store.EventRunLink):
			if child.str("superseded_by") == "" {
				tr.ActiveLink = child
			} else {
				tr.Superseded = append(tr.Superseded, child)
			}
		case string(store.EventRunObserved):
			if tr.Observed == nil || child.ts().After(tr.Observed.ts()) {
				tr.Observed = child
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(tr.Superseded, func(i, j int) bool { return tr.Superseded[i].ts().Before(tr.Superseded[j].ts()) })

	tr.Coverage = coverageOf(tr)
	return tr, tr.verify(ctx, db)
}

// verifiedEvents is every event this trace holds, in print order. It is the
// one list used for verifying, for printing and for the exit code, so a
// verdict can never be computed for an event that is not shown, nor an event
// shown without one.
func (tr *traceResult) verifiedEvents() []eventObj {
	out := make([]eventObj, 0, 5+len(tr.Superseded))
	for _, e := range []eventObj{tr.Rejected, tr.Trigger, tr.Outcome, tr.ActiveLink, tr.Observed} {
		if e != nil && e.eventID() != "" {
			out = append(out, e)
		}
	}
	for _, e := range tr.Superseded {
		if e != nil && e.eventID() != "" {
			out = append(out, e)
		}
	}
	return out
}

// verify recomputes content_sha256 for every event in the trace.
//
// The recomputation lives in internal/store (VerifyContentHash) because the
// hash scope is a CLOSED per-type field list built by unexported functions. A
// second implementation here would compile, pass vet, and drift the first time
// a field moved — and its failure mode is a confident MISMATCH on a healthy
// row, which sends an operator down the restore-from-backup path for nothing.
//
// A verification error is fatal to the command rather than a fourth verdict:
// the three verdicts are answers about the ROW, and "the query failed" is not
// one of them.
func (tr *traceResult) verify(ctx context.Context, db *store.DB) error {
	tr.Integrity = make(map[string]store.IntegrityResult)
	for _, e := range tr.verifiedEvents() {
		id := e.eventID()
		if _, done := tr.Integrity[id]; done {
			continue
		}
		res, err := db.VerifyContentHash(ctx, id)
		if err != nil {
			return fmt.Errorf("trace: verifying content_sha256 of %s: %w", id, err)
		}
		tr.Integrity[id] = res
	}
	return nil
}

// integrityOf returns the printable verdict line for one event.
func (tr *traceResult) integrityOf(e eventObj) string {
	if e == nil {
		return integrityLine(store.IntegrityResult{})
	}
	return integrityLine(tr.Integrity[e.eventID()])
}

// coverageOf applies the coverage query's CASE ladder to a single trigger.
//
// `gap` is unreachable here by construction: a gap is a GitHub run that never
// went through the gateway, and a traced trigger did. The two reachable
// answers are `covered` (an active `exact` link) and `unknown` (anything
// weaker, including no link at all).
func coverageOf(tr *traceResult) string {
	if tr.ActiveLink != nil && tr.ActiveLink.str("resolution") == string(store.ResolutionExact) {
		return store.CoverageCovered
	}
	return store.CoverageUnknown
}

func (tr *traceResult) print(w io.Writer, history bool) {
	if tr.Rejected != nil {
		tr.printRejected(w)
		return
	}
	if tr.OrphanedParent != "" {
		fmt.Fprintf(w, "ORPHAN    %s references trigger %s, which is no longer in the log.\n",
			tr.Trigger.eventID(), tr.OrphanedParent)
		fmt.Fprintf(w, "          Retention purges a trigger and its children together; a child alone\n")
		fmt.Fprintf(w, "          means a purge was interrupted. The retention purge removes these on the next run.\n\n")
	}

	t := tr.Trigger
	base := t.ts()
	fmt.Fprintf(w, "TRIGGER   %-24s %s\n", t.eventID(), fmtTS(base))

	from := t.str("caller", "source_ip")
	if from == "" {
		// source_ip is redactable and `standpipe forget` NULLs it. An
		// absent value is a successful redaction, not a missing record.
		from = "(redacted or not recorded)"
	}
	fmt.Fprintf(w, "  caller      %s (%s) from %s\n",
		orDash(t.str("caller", "name")), orDash(t.str("caller", "token_id")), from)
	fmt.Fprintf(w, "  target      %s · %s · %s\n",
		orDash(t.str("target", "project")), orDash(t.str("target", "workflow")), orDash(t.str("target", "ref")))
	// The raw inputs are NOT stored — only the HMAC over them — so a
	// key count cannot be reconstructed and is not invented.
	fmt.Fprintf(w, "  inputs      %s  (validation: %s)\n",
		orDash(t.str("inputs_hash")), validationNote(t.str("inputs_validation")))
	fmt.Fprintf(w, "  idempotency %s  →  %s\n",
		orDash(t.str("idempotency_key")), orDash(t.str("idempotency_outcome")))
	fmt.Fprintf(w, "  correlation %-18s %s\n",
		orDash(t.str("correlation")), correlationNote(t.str("correlation")))
	fmt.Fprintf(w, "  decision    %s (%s)\n", orDash(t.str("decision", "backend")), orDash(t.str("decision", "reason")))
	fmt.Fprintf(w, "  integrity   %s\n", tr.integrityOf(t))

	// --- outcome
	if tr.Outcome == nil {
		fmt.Fprintf(w, "\nOUTCOME   — (none recorded: the request never completed, or the gateway died\n")
		fmt.Fprintf(w, "              between the reservation and the outcome; the crash-recovery sweep takes that row\n")
		fmt.Fprintf(w, "              to `unknown` rather than guessing)\n")
	} else {
		o := tr.Outcome
		fmt.Fprintf(w, "\nOUTCOME   %-24s %s\n", o.eventID(), rel(base, o.ts()))
		httpStatus := "-"
		if code, ok := o.num("outcome_http_status"); ok {
			httpStatus = fmt.Sprintf("http %d", code)
		}
		fmt.Fprintf(w, "  status      %-20s %s\n", orDash(o.str("outcome_status")), httpStatus)
		if reason := o.str("outcome_reason"); reason != "" {
			fmt.Fprintf(w, "  reason      %s\n", reason)
		}
		if ms, ok := o.num("outcome_upstream_duration_ms"); ok {
			fmt.Fprintf(w, "  upstream    %dms\n", ms)
		}
		// `forwarded` is the authoritative success signal,
		// not the numeric code. With tier 1 armed a success answers 200, not
		// 204, and a reader keying on the number gets a silent empty set.
		fmt.Fprintf(w, "  (outcome_status is the success signal, not the HTTP code)\n")
		fmt.Fprintf(w, "  integrity   %s\n", tr.integrityOf(o))
	}

	// --- run link
	if tr.ActiveLink == nil {
		fmt.Fprintf(w, "\nRUN LINK  %s\n", unresolvedNote(base))
	} else {
		l := tr.ActiveLink
		fmt.Fprintf(w, "\nRUN LINK  %-24s %s\n", l.eventID(), rel(base, l.ts()))
		if runID, ok := l.num("run_id"); ok {
			fmt.Fprintf(w, "  run_id      %-20d %s\n", runID, runURL(t.str("target", "project"), runID))
		} else {
			// resolution `none` carries a NULL run_id: the horizon elapsed and
			// nothing matched. The link exists to record that, not to hide it.
			fmt.Fprintf(w, "  run_id      — (none; the resolver recorded that no run could be matched)\n")
		}
		fmt.Fprintf(w, "  resolution  %-20s source: %s\n",
			orDash(l.str("resolution")), sourceNote(l.str("resolution_source")))
		fmt.Fprintf(w, "  integrity   %s\n", tr.integrityOf(l))
	}

	if history {
		if len(tr.Superseded) == 0 {
			fmt.Fprintf(w, "\nHISTORY   — (no superseded run_links; this trigger was linked once)\n")
		}
		for _, s := range tr.Superseded {
			fmt.Fprintf(w, "\nHISTORY   %-24s %s  superseded by %s\n", s.eventID(), rel(base, s.ts()), s.str("superseded_by"))
			if runID, ok := s.num("run_id"); ok {
				fmt.Fprintf(w, "  run_id      %d\n", runID)
			}
			fmt.Fprintf(w, "  resolution  %-20s source: %s\n", orDash(s.str("resolution")), sourceNote(s.str("resolution_source")))
			fmt.Fprintf(w, "  integrity   %s\n", tr.integrityOf(s))
		}
	} else if len(tr.Superseded) > 0 {
		fmt.Fprintf(w, "\n          (%d superseded run_link(s) hidden; --history shows them)\n", len(tr.Superseded))
	}

	// --- observed
	if tr.Observed == nil {
		// run_observed is written only when conclusion IS NOT NULL. Its
		// absence is an unfinished run, not an error — and it is not left blank.
		fmt.Fprintf(w, "\nOBSERVED  — (no conclusion yet; this event is written only once the run finishes)\n")
	} else {
		ob := tr.Observed
		fmt.Fprintf(w, "\nOBSERVED  %-24s %s\n", ob.eventID(), rel(base, ob.ts()))
		fmt.Fprintf(w, "  conclusion  %s\n", orDash(ob.str("conclusion")))
		fmt.Fprintf(w, "  integrity   %s\n", tr.integrityOf(ob))
	}

	fmt.Fprintf(w, "\nCOVERAGE  %s\n", tr.Coverage)
	if tr.Coverage == store.CoverageUnknown {
		fmt.Fprintf(w, "          `unknown` means the gateway forwarded this and cannot prove which run it became.\n")
		fmt.Fprintf(w, "          It is a real state, distinct from `gap` (never went through the gateway).\n")
	}
}

func (tr *traceResult) printRejected(w io.Writer) {
	r := tr.Rejected
	fmt.Fprintf(w, "REJECTED  %-24s %s\n", r.eventID(), fmtTS(r.ts()))
	from := r.str("caller", "source_ip")
	if from == "" {
		from = "(redacted or not recorded)"
	}
	fmt.Fprintf(w, "  caller      %s (%s) from %s\n",
		orDash(r.str("caller", "name")), orDash(r.str("caller", "token_id")), from)
	fmt.Fprintf(w, "  target      %s · %s · %s\n",
		orDash(r.str("target", "project")), orDash(r.str("target", "workflow")), orDash(r.str("target", "ref")))
	code := "-"
	if c, ok := r.num("outcome_http_status"); ok {
		code = fmt.Sprintf("http %d", c)
	}
	fmt.Fprintf(w, "  reason      %-20s %s\n", orDash(r.str("reject_reason")), code)
	if n, ok := r.num("suppressed_count"); ok && n > 0 {
		// A 429 window is coalesced into one row and the count is the
		// number of requests folded into it. Printing the row without the
		// count would understate the rejection by that factor.
		since := "?"
		if ms, ok := r.num("suppressed_since"); ok {
			since = fmtTS(time.UnixMilli(ms))
		}
		fmt.Fprintf(w, "  suppressed  %d further request(s) coalesced into this row since %s\n", n, since)
	}
	if txt := r.str("reason_text"); txt != "" {
		fmt.Fprintf(w, "  detail      %s\n", txt)
	}
	fmt.Fprintf(w, "  integrity   %s\n", tr.integrityOf(r))
	fmt.Fprintf(w, "\n          A trigger_rejected row is parentless and has no outcome, run_link or\n")
	fmt.Fprintf(w, "          run_observed. It proves the refusal was RECORDED — \"silent dropping is\n")
	fmt.Fprintf(w, "          FORBIDDEN\" made testable. It does not enter the coverage query.\n")
}

// json is the --json output.
func (tr *traceResult) json(history bool) map[string]any {
	out := map[string]any{}
	// One integrity object per block, mirroring the keys below, so a consumer
	// reads the verdict next to the event it is about. Absent blocks are
	// explicit nulls for the same reason the event keys are.
	integrity := map[string]any{
		"trigger":      tr.integrityJSON(tr.Trigger),
		"outcome":      tr.integrityJSON(tr.Outcome),
		"run_link":     tr.integrityJSON(tr.ActiveLink),
		"run_observed": tr.integrityJSON(tr.Observed),
		"rejected":     tr.integrityJSON(tr.Rejected),
	}
	out["integrity"] = integrity
	if tr.Rejected != nil {
		out["rejected"] = map[string]any(tr.Rejected)
		return out
	}
	out["trigger"] = map[string]any(tr.Trigger)
	out["coverage"] = tr.Coverage
	if tr.OrphanedParent != "" {
		out["orphaned_parent"] = tr.OrphanedParent
	}
	// Absent facts are explicit nulls rather than absent keys: a consumer that
	// tests for a missing key cannot tell "no run_link" from "this field was
	// renamed".
	out["outcome"] = nilOr(tr.Outcome)
	out["run_link"] = nilOr(tr.ActiveLink)
	out["run_observed"] = nilOr(tr.Observed)
	if history {
		sup := make([]map[string]any, 0, len(tr.Superseded))
		supInteg := make([]any, 0, len(tr.Superseded))
		for _, s := range tr.Superseded {
			sup = append(sup, map[string]any(s))
			supInteg = append(supInteg, tr.integrityJSON(s))
		}
		out["superseded_run_links"] = sup
		integrity["superseded_run_links"] = supInteg
	} else {
		out["superseded_run_link_count"] = len(tr.Superseded)
	}
	if tr.ActiveLink == nil && tr.Trigger != nil {
		out["resolver_horizon_expires_at"] = tr.Trigger.ts().Add(forge.DefaultResolverHorizon).UTC().Format(time.RFC3339)
	}
	return out
}

// integrityJSON is one event's integrity verdict, machine-readable.
//
// `reason` and `detail` are present ONLY when they mean something: a reason on
// an `ok` verdict would invite a consumer to branch on it. `computed` is
// likewise absent unless a recomputation actually happened — printing a value
// there would suggest a comparison that did not take place.
func (tr *traceResult) integrityJSON(e eventObj) any {
	if e == nil {
		return nil
	}
	r := tr.Integrity[e.eventID()]
	obj := map[string]any{
		"event_id":       e.eventID(),
		"content_sha256": string(r.Verdict),
	}
	if r.Reason != "" {
		obj["reason"] = string(r.Reason)
	}
	if r.Detail != "" {
		obj["detail"] = r.Detail
	}
	if r.Stored != "" {
		obj["stored"] = r.Stored
	}
	if r.Computed != "" {
		obj["computed"] = r.Computed
	}
	return obj
}

func nilOr(e eventObj) any {
	if e == nil {
		return nil
	}
	return map[string]any(e)
}

// unresolvedNote is the explicit "this is missing" line for a trigger with
// no active run_link, including how long the resolver still has.
func unresolvedNote(triggerTS time.Time) string {
	deadline := triggerTS.Add(forge.DefaultResolverHorizon)
	left := time.Until(deadline)
	if left > 0 {
		return fmt.Sprintf("— (unresolved, tier 3 pending, horizon expires in %s)", humanDuration(left))
	}
	return fmt.Sprintf("— (unresolved, horizon elapsed %s ago; the resolver should have written resolution `none`)",
		humanDuration(-left))
}

// correlationNote spells out what the `correlation` field does and does not
// claim. The field records what the dispatch was ARMED with, before the
// dispatch — never which tier ended up establishing the link. That is
// run_link.resolution_source, and conflating the two is the mistake this note
// exists to prevent.
func correlationNote(c string) string {
	switch store.Correlation(c) {
	case store.CorrelationInjected:
		return "(tier 2 armed: standby_trigger_id was injected; tier 1 also requested)"
	case store.CorrelationDispatchResponse:
		return "(tier 1 only: no injection — the workflow declares no standby_trigger_id, or the input ceiling was reached)"
	case store.CorrelationWindow:
		return "(neither tier armed: github.return_run_details is false — only time-window correlation remains)"
	default:
		return ""
	}
}

// sourceNote maps run_link's resolution_source to the tier that produced the link.
func sourceNote(s string) string {
	switch store.ResolutionSource(s) {
	case store.SourceDispatchResponse:
		return "dispatch_response  (tier 1)"
	case store.SourceRunName:
		return "run_name  (tier 2 — the operator's standby_trigger_id did its job)"
	case store.SourceWindow:
		return "window  (tier 3 — a time-window inference, not a proof)"
	case "":
		return "— (not recorded)"
	default:
		return s
	}
}

func validationNote(v string) string {
	if store.InputsValidation(v) == store.ValidationSkippedColdCache {
		return v + "  ← validation was SKIPPED: the workflow definition was never fetched"
	}
	if v == "" {
		return "-"
	}
	return v
}

// integrityLine renders the three-valued integrity verdict.
//
// THE THIRD VALUE ALWAYS CARRIES ITS REASON: UNVERIFIABLE is never
// printed without its reason. A bare "unverifiable" is the same failure as a
// two-valued check — it tells the operator a question went unanswered without
// telling them which question, and the three reasons call for three different
// responses (nothing, restore, compare against retention).
func integrityLine(r store.IntegrityResult) string {
	switch r.Verdict {
	case store.IntegrityOK:
		return "content_sha256 OK"
	case store.IntegrityMismatch:
		return "content_sha256 MISMATCH — " + orText(r.Detail,
			"this row does not hash to its recorded digest")
	case store.IntegrityUnverifiable:
		if r.Reason == "" {
			// Unreachable through VerifyContentHash, which always sets a
			// reason. If it ever is reached, the verifier is the thing that is
			// broken and saying so beats printing a verdict with no grounds.
			return "content_sha256 UNVERIFIABLE (no reason recorded — that is a fault in the verifier, not a verdict about this row)"
		}
		return fmt.Sprintf("content_sha256 UNVERIFIABLE (%s) — %s", r.Reason,
			orText(r.Detail, "this build cannot recompute this row's hash scope"))
	default:
		// The zero IntegrityResult: no event, so nothing was asked.
		return "content_sha256 — (no event to verify)"
	}
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// runURL builds the browser address of a run.
//
// GitHub Enterprise Server is the caveat and `github.base_url` does NOT
// close it. That key
// names the API root; this is the browser root, and on GHES the two are
// different hosts (https://ghes.example/api/v3 against https://ghes.example).
// Deriving one from the other would be a guess about a deployment's topology,
// so github.com stays hardcoded and a GHES operator reads the run_id instead —
// an accepted residual limit, and the remaining half of the GHES gap.
func runURL(repo string, runID int64) string {
	if repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, runID)
}

func fmtTS(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05.000Z")
}

// rel renders a child's timestamp as an offset from the trigger, which is the
// question an operator actually has: how long did this take.
func rel(base, t time.Time) string {
	if base.IsZero() || t.IsZero() {
		return fmtTS(t)
	}
	return "+" + humanDuration(t.Sub(base))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
