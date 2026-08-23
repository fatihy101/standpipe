package exec

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// Plan is the matrix-expanded model plan (the validator's set P).
//
// The workflow model itself is NOT designed in the executor design and is not
// designed here. This type carries exactly the three things the validator
// and the roll-up require and
// nothing else, so that a later slice that does design the model is free:
//
//   - the plan record, (workflow_path, job_id, leg_key);
//   - roll-up rule 3's job-level `continue-on-error`, which is read FROM THE MODEL
//     because model.Job in the engine has no such field at all;
//   - the model-derived expected conclusion, which is how an `if:`-false or
//     `needs:`-blocked job becomes `skipped` without the engine being asked.
type Plan struct {
	WorkflowPath string
	Jobs         []PlanJob
}

// PlanJob is one matrix-expanded leg of the plan.
type PlanJob struct {
	JobID string

	// Leg is nil when the job has no matrix. This differs from
	// JobResult.MatrixLeg's nil, which means "could not tell" — the model
	// always knows.
	Leg *LegKey

	// ContinueOnError is roll-up rule 3's input.
	ContinueOnError bool

	// Expected is the conclusion the MODEL predicts, when it can predict one.
	// Empty means "the model expects this job to run and does not predict the
	// outcome".
	//
	// The limit: a job may only be counted `skipped` if its `if:` was
	// EVALUABLE, which depends on the event payload being complete. An `if:`
	// that could not be evaluated is not `skipped` — the run is unmeasurable.
	// Otherwise an empty payload read through `github.event.*` gets confirmed
	// as "skipped" BY THE MODEL, and the empty-payload class (both sides report
	// success while the guarded steps never ran) comes back from inside the
	// validator.
	Expected Conclusion
}

// LegKey is a matrix leg's canonical identity.
//
// The engine and the model must compute the SAME key or the join silently
// pairs the wrong rows, so the canonicalisation lives here and both sides call
// it. CanonicalStepID states the rule for step identity; this is its sibling
// one level up.
type LegKey struct {
	// Canonical is the serialised form: a JSON object with keys sorted and
	// values kept in their ORIGINAL JSON TYPE. Typing matters — the engine
	// emits a typed matrix object, so `1` and `"1"` are different legs and must
	// not canonicalise the same.
	//
	// "" is a legal value and means "an empty matrix object", i.e. a job with
	// no matrix. That is the ledger's leg_key = '' with leg_resolved = 1.
	Canonical string
}

// String returns the canonical form.
func (l *LegKey) String() string {
	if l == nil {
		return ""
	}
	return l.Canonical
}

// Resolved reports whether the leg was distinguished at all (the ledger's
// leg_resolved). A nil LegKey is unresolved; everything else, empty canonical
// included, is resolved.
func (l *LegKey) Resolved() bool { return l != nil }

// CanonicalLegKey builds a LegKey from a decoded matrix object.
//
// values maps each matrix dimension to its value as RAW JSON, so the type
// survives. A nil map yields nil: "no matrix object was emitted" is not the
// same fact as "the matrix object was empty".
func CanonicalLegKey(values map[string]json.RawMessage) *LegKey {
	if values == nil {
		return nil
	}
	if len(values) == 0 {
		return &LegKey{Canonical: ""}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil { // a Go string always marshals
			continue
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(compactJSON(values[k]))
	}
	b.WriteByte('}')
	return &LegKey{Canonical: b.String()}
}

// CanonicalLegKeyFromValues is CanonicalLegKey for a caller holding Go values
// rather than raw JSON — the model side. It marshals each value first so that
// both sides go through the same serialiser.
func CanonicalLegKeyFromValues(values map[string]any) *LegKey {
	if values == nil {
		return nil
	}
	raw := make(map[string]json.RawMessage, len(values))
	for k, v := range values {
		b, err := json.Marshal(v)
		if err != nil {
			b = []byte("null")
		}
		raw[k] = json.RawMessage(b)
	}
	return CanonicalLegKey(raw)
}

func compactJSON(in json.RawMessage) []byte {
	var out bytes.Buffer
	if err := json.Compact(&out, in); err != nil {
		// Not valid JSON. Keep the bytes rather than dropping them: a key that
		// silently loses a dimension pairs the wrong legs.
		return in
	}
	return out.Bytes()
}

// CanonicalStepID joins a multi-element step identity into the ledger's
// single-column form. ["g","mk"] becomes "g.mk" — the inner step of composite "g".
//
// It is the join key, and it is NOT the declared ordinal: an engine emits the
// declared `id:` when one was given and the declared index only when one was
// not. A validator has to reproduce that rule on the model
// side; it must not read it back off the engine.
func CanonicalStepID(id []string) string {
	if len(id) == 0 {
		return ""
	}
	return strings.Join(id, ".")
}
