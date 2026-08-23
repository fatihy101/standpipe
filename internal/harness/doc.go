// Package harness is the layer ABOVE exec.Executor: the four-part conclusion
// validator, its ordered ten-row verdict (Validate), and the call into the
// run-level conclusion derivation.
//
// WHY IT IS ABOVE THE INTERFACE, AND NOT INSIDE AN ENGINE. Three of the
// validator's four parts never see an engine at all: (a) the plan comes FROM
// THE MODEL, (b) the `skipped` mapping is made FROM THE MODEL, and (d) the
// event payload is what the harness itself produced. Only (c) needs the
// engine's observation. Putting the validator inside an engine would mean
// rewriting it whenever the engine changed — exactly what the executor
// interface exists to prevent ("swapping the motor costs a SLICE, not a
// PHASE").
//
// THE VALIDATOR'S SINGLE SENTENCE, and every line here is downstream of it:
//
//	the engine's output is a MEASUREMENT RESULT, never a plan source.
//
// The original validator was three things at once — necessary, insufficient,
// and wrong in the strict direction — and all three came from the one mistake
// of validating against the engine's output. This package therefore reads the
// plan, the expected `skipped` set, the declared step identities and the event
// payload from the MODEL, and uses the engine only for what was observed.
//
// THREE SETS. P is the model's matrix-expanded plan; G is GitHub's job set; A
// is the engine's Result. `act --list` is not a source of P: it reports at
// job-ID granularity and a validator fed from it never sees a matrix leg.
//
// THE ORDER IS PART OF THE DECISION. VALIDATION BEFORE DERIVATION — without
// that ordering a run that executed zero jobs reaches the derivation's
// "all jobs succeeded/skipped" rules, acquires a conclusion, and is scored as
// a match. That false match is the entire reason the validator exists.
//
// THE VOCABULARY IS CLOSED. Every verdict this package produces is one of the
// five exec outcomes with one of its fourteen refusal reasons. A situation the
// set cannot name is a finding for the maintainers, never a fifteenth
// constant. Diagnostics that have no member to name them go into
// Verdict.Findings, which is the same relief valve exec.Provenance.Notes is,
// and is never a substitute for a refusal.
//
// EVIDENCE DISCIPLINE. Nothing here branches on a behaviour whose only
// evidence is [S] (read in the engine's source) or [V] (stated by a vendor's
// docs); a branch needs [M], a measurement. In particular the engine's
// `stepResult` vocabulary is closed by [S] and observed by [M]; both markers
// stay, and an unrecognised value becomes exec.ConclusionUnmappable rather
// than a guess.
package harness
