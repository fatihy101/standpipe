package exec

import "fmt"

// DeriveRunConclusion is the run-level conclusion roll-up.
//
// It lives above the interface, in the harness, because the rule is GitHub's
// semantics and not any engine's — an engine produces per-job results and no
// run-level result at all. Putting it here means changing engines costs a
// slice, not a phase.
//
// RULE 1 IS THE CALLER'S AND IS NOT IMPLEMENTED HERE. "The validator did
// not pass" means no derivation happens at all and the Outcome is already not
// completed; a function that could be handed a failed validation and still
// return a conclusion would be exactly the false match the validator exists to prevent
// (a run where zero jobs ran passing through rule 5 or 6). Call this ONLY after
// the validator's verdict steps 1-9 have passed, and feed the answer to Result.Finalise.
//
// Rules 2-6, first match wins:
//
//  2. any job cancelled                                        -> cancelled
//  3. any job failure whose model continue-on-error is not true -> failure
//  4. any job timed_out                                        -> failure
//  5. all jobs skipped                                         -> skipped
//  6. otherwise                                                -> success
//
// continueOnError is keyed by JobID. It is read FROM THE MODEL: the engine's
// job type has no such field, so it cannot be read from the run. A job absent
// from the map counts as false, which is the conservative direction — a failure
// stays a failure.
//
// An empty job set is an error rather than rule 5 or 6. A run with no jobs is
// the shape of act's silent skip on an unsupported platform (no job runs, no
// result is written, the process exits 0), and the whole reason rule 1 orders validation before
// derivation is that such a run must never acquire a conclusion.
func DeriveRunConclusion(jobs []JobResult, continueOnError map[string]bool) (Conclusion, error) {
	if len(jobs) == 0 {
		return "", fmt.Errorf("exec: no jobs — a run with an empty job set must not acquire a conclusion (rule 1)")
	}

	sawCancelled := false
	sawFailure := false
	sawTimedOut := false
	allSkipped := true

	// The whole set is scanned before any rule fires: the rules are predicates
	// over the set ("at least one job ..."), and returning early would also
	// skip the vocabulary check on the jobs after the match.
	for _, j := range jobs {
		if !j.Conclusion.Valid() {
			return "", fmt.Errorf("exec: job %q conclusion %q is outside the vocabulary", j.JobID, j.Conclusion)
		}
		if j.Conclusion == ConclusionUnmappable {
			// Verdict step 9 (conclusion mapping) should already have caught this. Reaching derivation
			// with an unmappable job means the order was not followed, and
			// guessing here is the one thing the interface forbids.
			return "", fmt.Errorf("exec: job %q is unmappable — the conclusion-mapping step must have run first", j.JobID)
		}
		if j.Conclusion != ConclusionSkipped {
			allSkipped = false
		}
		switch j.Conclusion {
		case ConclusionCancelled:
			sawCancelled = true
		case ConclusionFailure:
			if !continueOnError[j.JobID] {
				sawFailure = true
			}
		case ConclusionTimedOut:
			sawTimedOut = true
		}
	}

	switch {
	case sawCancelled:
		return ConclusionCancelled, nil // rule 2
	case sawFailure:
		return ConclusionFailure, nil // rule 3
	case sawTimedOut:
		return ConclusionFailure, nil // rule 4 — GitHub calls this run failed
	case allSkipped:
		return ConclusionSkipped, nil // rule 5
	default:
		return ConclusionSuccess, nil // rule 6
	}
}
