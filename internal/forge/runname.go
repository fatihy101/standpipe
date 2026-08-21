package forge

import (
	"strings"

	"github.com/google/go-github/v90/github"
)

// ---------------------------------------------------------------------------
// SEAM 2 of 3 — which workflow-run field carries run-name. VERIFIED DEFAULT,
// escape hatch kept.
//
// ANSWERED BY MEASUREMENT on 2026-08-21, ON OUR OWN EVENT TYPE. The tier-2
// "unverified" marker is CLOSED. Do not reopen it as an
// investigation; changing anything here needs a NEW measurement.
//
// WHAT WAS MEASURED ON workflow_dispatch. Four real dispatches against a private throwaway repo whose workflow
// `name:` was "probe-workflow-name" while its `run-name:` was
// "probe • ${{ inputs.standby_trigger_id }}" — deliberately different strings,
// so the two fields could be told apart. All four runs (32503948002,
// 32503921890, 32503920102, 32503859910) had the identical shape:
//
//   - `run-name` populates BOTH `name` AND `display_title` with the same
//     string, and the workflow's own `name:` appears in NEITHER.
//   - Therefore which field is read cannot change a match, and
//     RunNameFieldEither is correct AT ZERO COST. The constant did not have to
//     move and the code was not touched by that slice.
//   - The injected id travelled the whole tier-2 chain end to end: injection ->
//     run-name template interpolation -> the run's fields.
//
// EARLIER, ON OTHER EVENT TYPES (read-only `gh` against public repos, same
// day). Consistent with the above: hanami/hanami's tag push carried the
// run-name in both fields; five sc-im runs across `push` and `pull_request`
// carried it in both fields, confirming actions/runner#4141's conflation on
// the REST surface rather than only on the webhook payload.
//
// WHAT IS STILL NOT KNOWN — DO NOT DELETE THIS, IT IS NOT THE OLD QUESTION.
// `run-name` APPLICATION VARIES BY EVENT TYPE, measured: a sc-im
// `pull_request` run on 2026-08-17 carried the run-name in NEITHER field —
// name="CI", display_title="Guard use of wordexp macros." — while the workflow
// file AT THAT RUN'S EXACT head_sha declared `run-name: continuous
// integration`. The declaration was there and the value was not. No cause is
// known. That variance was never measured for `workflow_dispatch` and is not
// covered by the four probe runs: they are one repo, one workflow file, one
// day. "GitHub always applies run-name" is NOT an established fact.
//
// WHAT THIS FILE STILL CANNOT FIX. If GitHub does not apply `run-name` to a
// run, no field carries the trigger id and tier 2 cannot match however many
// fields it reads. That case had no symptom in the resolver at all; it now has
// one, and it STAYS for exactly the reason above — see tier2Absent and
// gateway_run_name_absent_total in resolver.go. If application ever starts
// varying on `workflow_dispatch` too, a distribution shift is the only trace
// and that detector is the only thing that would show it.
//
// TO CORRECT: change RunNameSource below (one constant). Every correlation
// path reads the run-name through runNameValues; nothing else in the package
// touches DisplayTitle or Name. The seam stays because GitHub can change under
// us, not because the answer is in doubt.
// ---------------------------------------------------------------------------

// RunNameField names the workflow-run field read as the run-name.
type RunNameField int

const (
	// RunNameFieldDisplayTitle reads display_title only. This is what
	// the design originally recorded as the assumption. Measured to be
	// sufficient on every run observed, including all four workflow_dispatch
	// probe runs — and still narrower than the default, because it misses the
	// actions/runner#4141 shape where `name` carries the run-name.
	RunNameFieldDisplayTitle RunNameField = iota
	// RunNameFieldName reads name only. Also measured to be sufficient on the
	// probe runs, and narrow for the mirror-image reason.
	RunNameFieldName
	// RunNameFieldEither reads BOTH fields and matches on either. This is the
	// default, and 2026-08-21's measurement makes it free rather than merely
	// safe: on workflow_dispatch the two fields carry the SAME string, so the
	// second read is one extra comparison on a deduplicated slice and can
	// never change a match. It is kept as the default because a match that is
	// missed produces NO ERROR anywhere — the trigger just falls to tier 3 and
	// is written `inferred` — while a widened match can at worst produce
	// `ambiguous`, which is recorded.
	RunNameFieldEither
)

// RunNameSource is the single switch. See the block comment above: the value
// is measured-correct as of 2026-08-21, not assumed.
const RunNameSource = RunNameFieldEither

// runNameValues returns every field that may carry the run-name under
// RunNameSource, in the order they should be preferred for display. Empty
// fields are dropped, so the result is empty when the run carries no name at
// all.
//
// This is the only place in the package that reads DisplayTitle or Name.
func runNameValues(run *github.WorkflowRun) []string {
	if run == nil {
		return nil
	}
	var fields []string
	switch RunNameSource {
	case RunNameFieldName:
		fields = []string{run.GetName()}
	case RunNameFieldEither:
		fields = []string{run.GetDisplayTitle(), run.GetName()}
	default:
		fields = []string{run.GetDisplayTitle()}
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f == "" {
			continue
		}
		// Measured 2026-08-21, on all four workflow_dispatch probe runs: the
		// two fields hold the SAME string. Deduplicating makes the measured
		// case cost one comparison rather than two.
		if len(out) > 0 && out[0] == f {
			continue
		}
		out = append(out, f)
	}
	return out
}

// RunNameOf returns the run-name value of a workflow run for display and
// logging, or "" if the run carries none.
//
// Under RunNameFieldEither more than one field may be readable; this returns
// the preferred one. It is NOT the matching path — correlation goes through
// RunCarriesTriggerID, which tests every field, because picking one field to
// display and one field to match on are different decisions.
func RunNameOf(run *github.WorkflowRun) string {
	values := runNameValues(run)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// RunCarriesTriggerID reports whether a run's run-name contains the given
// trigger event ID, in ANY field RunNameSource admits.
//
// Substring, not equality: the documented run-name template surrounds the ID with
// operator-chosen text ("release • <id>"), so the gateway cannot predict the
// whole string. Event IDs are ULID/UUIDv7, long enough that an
// accidental substring hit is not a practical concern — which is also why
// testing both fields instead of one is close to free of false-positive risk.
func RunCarriesTriggerID(run *github.WorkflowRun, triggerEventID string) bool {
	if triggerEventID == "" {
		return false
	}
	for _, v := range runNameValues(run) {
		if strings.Contains(v, triggerEventID) {
			return true
		}
	}
	return false
}
