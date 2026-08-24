package exec

import (
	"encoding/json"
	"strings"
	"testing"
)

// The run-level roll-up and the two join keys (leg and step). Both are places where a
// wrong implementation produces a legal-looking answer and no error at all.

// TestDeriveRunConclusionRules walks roll-up rules 2-6 and their precedence.
func TestDeriveRunConclusionRules(t *testing.T) {
	job := func(id string, c Conclusion) JobResult {
		return JobResult{JobID: id, Conclusion: c, Steps: []StepObservation{}}
	}
	for _, tc := range []struct {
		name string
		jobs []JobResult
		coe  map[string]bool
		want Conclusion
	}{
		{"rule 6 — all success", []JobResult{job("a", ConclusionSuccess)}, nil, ConclusionSuccess},
		{"rule 5 — all skipped", []JobResult{job("a", ConclusionSkipped), job("b", ConclusionSkipped)}, nil, ConclusionSkipped},
		{"rule 5 does not fire on a mix", []JobResult{job("a", ConclusionSkipped), job("b", ConclusionSuccess)}, nil, ConclusionSuccess},
		{"rule 3 — a failure", []JobResult{job("a", ConclusionSuccess), job("b", ConclusionFailure)}, nil, ConclusionFailure},
		{"rule 4 — timed_out is a run FAILURE", []JobResult{job("a", ConclusionTimedOut)}, nil, ConclusionFailure},
		{"rule 2 beats rule 3", []JobResult{job("a", ConclusionFailure), job("b", ConclusionCancelled)}, nil, ConclusionCancelled},
		{"rule 2 beats rule 4", []JobResult{job("a", ConclusionTimedOut), job("b", ConclusionCancelled)}, nil, ConclusionCancelled},
		{"rule 3 beats rule 5", []JobResult{job("a", ConclusionSkipped), job("b", ConclusionFailure)}, nil, ConclusionFailure},

		// Rule 3's input is read FROM THE MODEL. The engine's job type has
		// no continue-on-error field, so it cannot be read from the run.
		{"continue-on-error excuses the failure", []JobResult{job("a", ConclusionFailure)}, map[string]bool{"a": true}, ConclusionSuccess},
		{"...only for the job the model named", []JobResult{job("a", ConclusionFailure), job("b", ConclusionFailure)}, map[string]bool{"a": true}, ConclusionFailure},
		{"a job absent from the map counts as false", []JobResult{job("a", ConclusionFailure)}, map[string]bool{"other": true}, ConclusionFailure},
		// continue-on-error does not reach timed_out or cancelled.
		{"it does not excuse timed_out", []JobResult{job("a", ConclusionTimedOut)}, map[string]bool{"a": true}, ConclusionFailure},
		{"it does not excuse cancelled", []JobResult{job("a", ConclusionCancelled)}, map[string]bool{"a": true}, ConclusionCancelled},

		{"a neutral job is neither skipped nor a failure", []JobResult{job("a", ConclusionNeutral)}, nil, ConclusionSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveRunConclusion(tc.jobs, tc.coe)
			if err != nil {
				t.Fatalf("DeriveRunConclusion: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeriveRunConclusionRefusesToConcludeNothing is the silent-skip shape: a
// run in which no job ran must NEVER acquire a conclusion. Rules 5 and 6 both have an
// answer for an empty set, and either one would score zero work as a match.
func TestDeriveRunConclusionRefusesToConcludeNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		jobs []JobResult
	}{
		{"nil", nil},
		{"empty", []JobResult{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeriveRunConclusion(tc.jobs, nil)
			if err == nil {
				t.Fatalf("a run with no jobs concluded %q", got)
			}
			if got != "" {
				t.Fatalf("an error came back with a conclusion %q attached", got)
			}
		})
	}
}

// TestDeriveRunConclusionRefusesUnmappable — the conclusion-mapping step must
// have run first, and guessing here is the one thing the interface forbids. The whole set is scanned before
// any rule fires, so an unmappable job AFTER a matching one is still caught.
func TestDeriveRunConclusionRefusesUnmappable(t *testing.T) {
	jobs := []JobResult{
		{JobID: "a", Conclusion: ConclusionCancelled}, // rule 2 would match here
		{JobID: "b", Conclusion: ConclusionUnmappable},
	}
	if got, err := DeriveRunConclusion(jobs, nil); err == nil {
		t.Fatalf("an unmappable job produced conclusion %q; rule 2 returned early and skipped the vocabulary check", got)
	}
	// And a value outside the vocabulary entirely.
	if got, err := DeriveRunConclusion([]JobResult{{JobID: "a", Conclusion: "in_progress"}}, nil); err == nil {
		t.Fatalf("a status was accepted as a conclusion: %q", got)
	}
}

// TestCanonicalLegKeyDistinguishesAbsentFromEmpty: act's matrix job results
// bleed across legs, so legs may be indistinguishable. nil is a MEASUREMENT
// OUTCOME — "the engine could not tell the legs apart" — and an empty matrix
// object is a job with no matrix. Collapsing them makes two different facts one
// row, and for act 1 is expected while 0 is an event.
func TestCanonicalLegKeyDistinguishesAbsentFromEmpty(t *testing.T) {
	absent := CanonicalLegKey(nil)
	if absent != nil {
		t.Fatalf("a nil matrix produced %+v; nil means the legs could not be told apart", absent)
	}
	if absent.Resolved() {
		t.Fatal("a nil LegKey reports itself resolved")
	}
	if absent.String() != "" {
		t.Fatalf("a nil LegKey stringified to %q", absent.String())
	}

	empty := CanonicalLegKey(map[string]json.RawMessage{})
	if empty == nil {
		t.Fatal("an empty matrix object produced nil; that is a job with no matrix, not an unresolved leg")
	}
	if !empty.Resolved() || empty.Canonical != "" {
		t.Fatalf("empty leg = %+v, want resolved with an empty canonical form", empty)
	}
}

// TestCanonicalLegKeyKeepsJSONTypes — the engine emits a TYPED matrix object, so
// 1 and "1" are different legs and must not canonicalise the same. A join that
// pairs them silently pairs the wrong rows.
func TestCanonicalLegKeyKeepsJSONTypes(t *testing.T) {
	number := CanonicalLegKey(map[string]json.RawMessage{"node": json.RawMessage(`1`)})
	str := CanonicalLegKey(map[string]json.RawMessage{"node": json.RawMessage(`"1"`)})
	if number.Canonical == str.Canonical {
		t.Fatalf("the typed and string legs canonicalise identically: %q", number.Canonical)
	}
	if number.Canonical != `{"node":1}` {
		t.Fatalf("number leg = %q", number.Canonical)
	}
	if str.Canonical != `{"node":"1"}` {
		t.Fatalf("string leg = %q", str.Canonical)
	}

	// Keys are SORTED, so the two sides compute the same key whatever order the
	// map iterated in.
	a := CanonicalLegKey(map[string]json.RawMessage{"os": json.RawMessage(`"linux"`), "node": json.RawMessage(`20`)})
	b := CanonicalLegKey(map[string]json.RawMessage{"node": json.RawMessage(`20`), "os": json.RawMessage(`"linux"`)})
	if a.Canonical != b.Canonical || a.Canonical != `{"node":20,"os":"linux"}` {
		t.Fatalf("key order leaks into the canonical form: %q vs %q", a.Canonical, b.Canonical)
	}

	// The model side must go through the same serialiser as the engine side, or
	// the two compute different keys for the same leg.
	fromValues := CanonicalLegKeyFromValues(map[string]any{"node": 20, "os": "linux"})
	if fromValues.Canonical != a.Canonical {
		t.Fatalf("the model side computes %q and the engine side %q", fromValues.Canonical, a.Canonical)
	}
	if CanonicalLegKeyFromValues(nil) != nil {
		t.Fatal("the model side turned an absent matrix into a resolved leg")
	}
}

// TestCanonicalStepIDIsTheJoinKey — ["g","mk"] is the inner step of composite
// ["g"], and the ledger once keyed the table on the ORDINAL until three records with no
// declared order between them collapsed onto one key.
func TestCanonicalStepIDIsTheJoinKey(t *testing.T) {
	got := map[string]bool{}
	for _, id := range [][]string{{"g"}, {"g", "mk"}, {"g", "1"}} {
		got[CanonicalStepID(id)] = true
	}
	if len(got) != 3 {
		t.Fatalf("g, g.mk and g.1 collapsed onto %d keys: %v", len(got), got)
	}
	if CanonicalStepID(nil) != "" {
		t.Fatal("an absent step identity produced a non-empty key")
	}
}

// TestConclusionRefusalsNameTheSetTheyEnforce pins the message, not just the
// verdict.
//
// Both IsGitHub() refusals in this package used to assert the set's SIZE — "one
// of GitHub's eight" — and both were still saying eight after `startup_failure`
// made it nine. The verdict was right the whole time; only the
// sentence an operator reads was wrong, which is why no test caught it. So this
// asserts that each message names every member of the set it enforces and
// states no size at all.
func TestConclusionRefusalsNameTheSetTheyEnforce(t *testing.T) {
	// `in_progress` is a status, never a conclusion, so both sites refuse it.
	empty := Result{Provenance: Provenance{InvocationID: "i"}}
	finaliseErr := empty.Finalise(OutcomeCompleted, "", Conclusion("in_progress"))
	stored := Result{
		Outcome:       OutcomeCompleted,
		RunConclusion: Conclusion("in_progress"),
		Provenance:    Provenance{InvocationID: "i"},
	}
	checkErr := stored.Check()
	if finaliseErr == nil || checkErr == nil {
		t.Fatalf("a status value was accepted as a conclusion (Finalise=%v, Check=%v)", finaliseErr, checkErr)
	}

	for site, msg := range map[string]string{"Finalise": finaliseErr.Error(), "Check": checkErr.Error()} {
		for _, c := range RunConclusions() {
			if !strings.Contains(msg, string(c)) {
				t.Errorf("%s's refusal does not name %q, which it would have accepted:\n\t%s", site, c, msg)
			}
		}
		// A size is a hand copy of the set. It is what went stale.
		for _, size := range []string{"eight", "nine", "ten", " 8 ", " 9 "} {
			if strings.Contains(msg, size) {
				t.Errorf("%s's refusal asserts the set's size (%q) instead of naming it:\n\t%s", site, size, msg)
			}
		}
	}
}

// TestFinaliseIsTheOnlyWayToConclude holds the ledger's CHECKs one level above the
// database, so a Result that could not be stored cannot be passed around either.
func TestFinaliseIsTheOnlyWayToConclude(t *testing.T) {
	for _, tc := range []struct {
		name       string
		verdict    Outcome
		reason     RefusalReason
		conclusion Conclusion
		wantErr    bool
	}{
		{"completed with a conclusion GitHub publishes", OutcomeCompleted, "", ConclusionSuccess, false},
		// The run-only member. Finalise and Check both read IsGitHub(), so a
		// row here covers both sites; without it the only conclusion either
		// site is exercised with is one that was legal before `startup_failure` was admitted.
		{"completed with startup_failure", OutcomeCompleted, "", ConclusionStartupFailure, false},
		{"completed with unmappable", OutcomeCompleted, "", ConclusionUnmappable, true},
		{"completed with no conclusion", OutcomeCompleted, "", "", true},
		{"completed carrying a reason", OutcomeCompleted, RefusalTokenAbsent, ConclusionSuccess, true},

		// U-5: deadline_exceeded names its own cause and is NEVER failure.
		{"deadline_exceeded alone", OutcomeDeadlineExceeded, "", "", false},
		{"deadline_exceeded with a reason", OutcomeDeadlineExceeded, RefusalTokenAbsent, "", true},
		{"deadline_exceeded as a failure", OutcomeDeadlineExceeded, "", ConclusionFailure, true},

		{"refused with a reason", OutcomeRefused, RefusalTokenAbsent, "", false},
		{"refused with no reason", OutcomeRefused, "", "", true},
		{"refused with an invented reason", OutcomeRefused, RefusalReason("disk_full"), "", true},
		{"refused carrying a conclusion", OutcomeRefused, RefusalTokenAbsent, ConclusionFailure, true},
		{"harness_error with a reason", OutcomeHarnessError, RefusalJobWithoutResult, "", false},
		{"unmeasurable with a reason", OutcomeUnmeasurable, RefusalConclusionUnmappable, "", false},
		{"an outcome outside the set", Outcome("abandoned"), "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Result{Provenance: Provenance{InvocationID: "i"}}
			err := r.Finalise(tc.verdict, tc.reason, tc.conclusion)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Finalise accepted %+v", tc)
				}
				// A rejected verdict must not have been written anyway.
				if r.Outcome != "" || r.RefusalReason != "" || r.RunConclusion != "" {
					t.Fatalf("a rejected Finalise still mutated the Result: %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("Finalise: %v", err)
			}
			if err := r.Check(); err != nil {
				t.Fatalf("Finalise produced a Result that Check rejects: %v", err)
			}
		})
	}
}

// TestCheckRefusesANilStepSlice — U-4: an empty slice means "no step ran" and
// "I do not know" is a refusal. A nil slice standing in for ignorance is exactly
// a silent failure: the work never happens and nothing says so.
func TestCheckRefusesANilStepSlice(t *testing.T) {
	r := Result{
		Outcome:       OutcomeCompleted,
		RunConclusion: ConclusionSuccess,
		Provenance:    Provenance{InvocationID: "i"},
		Jobs:          []JobResult{{JobID: "a", Conclusion: ConclusionSuccess, Steps: nil}},
	}
	if err := r.Check(); err == nil {
		t.Fatal("Check accepted a job with a nil step slice")
	}
	r.Jobs[0].Steps = []StepObservation{}
	if err := r.Check(); err != nil {
		t.Fatalf("Check rejected an empty step slice, which means 'no step ran': %v", err)
	}
	// U-6: a Result may not be returned without provenance.
	r.Provenance.InvocationID = ""
	if err := r.Check(); err == nil {
		t.Fatal("Check accepted a Result with no provenance")
	}
}

// TestRequestValidateRejectsAnEmptyPayload: an engine that runs on an
// empty payload makes every `github.event.*` guard false, the guarded steps
// vanish from the log entirely, and both sides conclude success having done none
// of the work.
func TestRequestValidateRejectsAnEmptyPayload(t *testing.T) {
	good := Request{
		Repo: "o/n", CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		WorkflowPath: ".github/workflows/t.yml", EventName: "push",
		EventPayload: []byte(`{"ref":"refs/heads/main"}`),
		WorkDir:      "/w", RunsOn: []string{"ubuntu-24.04"},
		SupplyMode: SupplyOnline, Purpose: PurposeDrill, Deadline: 1,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a complete Request was rejected: %v", err)
	}
	for _, tc := range []struct {
		name  string
		mutit func(*Request)
	}{
		{"an empty event payload", func(r *Request) { r.EventPayload = nil }},
		{"no runs-on labels", func(r *Request) { r.RunsOn = nil }},
		{"no purpose — it has no default", func(r *Request) { r.Purpose = "" }},
		{"no supply mode — the gate's score axis", func(r *Request) { r.SupplyMode = "" }},
		{"no deadline (U-5)", func(r *Request) { r.Deadline = 0 }},
		{"a short commit sha", func(r *Request) { r.CommitSHA = "0123456" }},
		{"no work directory", func(r *Request) { r.WorkDir = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := good
			tc.mutit(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
}
