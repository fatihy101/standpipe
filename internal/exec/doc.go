// Package exec is Phase 1's executor interface.
//
// It sits BELOW the harness and ABOVE any engine. The harness owns the
// workflow model, the validator and the run-level conclusion derivation; an
// engine implementation owns image provisioning, semantic normalisation and
// the actual invocation. This package is the seam, and it deliberately knows
// nothing about any particular engine: no engine concept (act's flags, image
// names, `.actrc`, `docker` itself) appears here.
//
// Three things in here are not signatures and cannot be read off one:
//
//  1. The implementation obligations U-1..U-8. An implementation that gets one
//     wrong is silently wrong, not loudly wrong. Implementations restate which
//     one they satisfy and how:
//     U-1 verify the execution ground (for act: the image claim) before
//     running; U-2 present GitHub's semantics whatever the engine's own are
//     (for act: the `defaults.run` replace rule); U-3 assert that WorkDir is
//     clean and at CommitSHA, and run the engine IN that directory; U-4 return
//     a structured job and step set, or say it cannot (never an empty slice);
//     U-5 enforce the wall-clock bound itself, and report it as
//     deadline_exceeded, never `failure`; U-6 record what it ran (Provenance);
//     U-7 never skip silently: every job and step not run has a result or a
//     refusal reason; U-8 record the resolved revision of every local source
//     mapping, or refuse.
//  2. The result vocabulary is CLOSED. Outcome has five members and
//     RefusalReason has fourteen; Comparison, the validator's verdict, has two.
//     A situation a set cannot name is a design finding, never a fifteenth
//     constant added in passing — and a mismatch's EXPLANATION is exactly such
//     an open finding, not a third Comparison. All SEVEN closed sets here are
//     enumerable at runtime — Outcomes, RefusalReasons, Conclusions,
//     RunConclusions, Comparisons, SupplyModes, Purposes — so that anything
//     holding a second copy of one (a schema CHECK, a counter catalogue) can be
//     checked against this package instead of trusted. A set left off that
//     list is a set the check silently does not cover, so the list is complete
//     or it is worthless.
//  3. What Run may and may not fill in. Run NEVER derives a run-level
//     conclusion — that is the harness's roll-up, above this interface. See
//     Result.
package exec
