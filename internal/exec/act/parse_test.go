package act

import (
	"strings"
	"testing"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Two measured properties of `act --json --verbose` that the design does not
// state and that a straightforward Go parser gets wrong SILENTLY. Both were
// found by reading the first measurement's stored logs on 2026-08-22; both proofs lived only
// in that reading until now.

// TestDecodeRecordDoesNotMergeStepidIntoStepID is the case collision.
//
// The injected pseudo-steps carry a LOWERCASE `stepid`. Go's encoding/json
// matches field names case-insensitively, so a struct tagged `json:"stepID"`
// also absorbs `stepid` and the injected records acquire a step identity they do
// not have. The stage=="Main" filter saves the current caller BY ACCIDENT — the
// injected records have no stage — so the guarantee is asserted on the decoder,
// not on the pipeline that happens to hide it.
func TestDecodeRecordDoesNotMergeStepidIntoStepID(t *testing.T) {
	const injected = `{"jobID":"build","msg":"⭐ Run Set up job","step":"Set up job","stepid":["--setup-job"]}`
	rec, ok := decodeRecord([]byte(injected))
	if !ok {
		t.Fatalf("the injected record did not decode at all")
	}
	if len(rec.stepID) != 0 {
		t.Fatalf("`stepid` was absorbed as a step identity: %v — a tagged struct is doing the decoding and every injected record now has an identity it does not have", rec.stepID)
	}
	if rec.jobID != "build" || rec.step != "Set up job" {
		t.Fatalf("the rest of the record was lost: %+v", rec)
	}

	// The exact spelling still counts.
	rec, ok = decodeRecord([]byte(`{"jobID":"build","stage":"Main","stepID":["0"],"step":"build","stepResult":"success"}`))
	if !ok || len(rec.stepID) != 1 || rec.stepID[0] != "0" {
		t.Fatalf("the declared record lost its identity: %+v", rec)
	}

	// And a record carrying BOTH spellings must take only the exact one. This
	// is the shape that makes the collision invisible: a merging decoder picks
	// whichever came last.
	rec, _ = decodeRecord([]byte(`{"jobID":"b","stepid":["--setup-job"],"stepID":["3"]}`))
	if len(rec.stepID) != 1 || rec.stepID[0] != "3" {
		t.Fatalf("stepID = %v, want [3]", rec.stepID)
	}
}

// TestParseStreamSurvivesNonJSONLines is the second property. act writes plain,
// non-JSON lines under --json — `Error: Job 'shard' failed` was seen verbatim in
// four of the measured corpora. A streaming json.Decoder errors at the first one and
// EVERYTHING AFTER IT IS LOST; in one workflow that line was at line 62 of 63, so the
// loss would have been invisible in that corpus and total in another.
func TestParseStreamSurvivesNonJSONLines(t *testing.T) {
	stream := strings.Join([]string{
		`{"jobID":"shard","stage":"Main","stepID":["0"],"step":"one","stepResult":"success"}`,
		`Error: Job 'shard' failed`,
		``,
		`  not json either  `,
		`{"jobID":"shard","stage":"Main","stepID":["1"],"step":"two","stepResult":"failure"}`,
		`{"jobID":"shard","jobResult":"failure"}`,
	}, "\n")

	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if obs.nonJSONLines != 2 {
		t.Fatalf("nonJSONLines = %d, want 2", obs.nonJSONLines)
	}
	jobs, without := obs.jobResults()
	if len(without) != 0 {
		t.Fatalf("jobs without a result: %v", without)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if n := len(jobs[0].Steps); n != 2 {
		t.Fatalf("got %d steps, want 2 — the records after the plain-text line were lost", n)
	}
	if jobs[0].Steps[1].Name != "two" || jobs[0].Steps[1].Conclusion != exec.ConclusionFailure {
		t.Fatalf("the step after the plain-text line is wrong: %+v", jobs[0].Steps[1])
	}
	if jobs[0].Conclusion != exec.ConclusionFailure {
		t.Fatalf("job conclusion = %q, want failure", jobs[0].Conclusion)
	}
}

// TestUnsupportedPlatformIsFoundOnAPlainLine — verdict step 4's hard harness
// error.
// No job runs, no result is written and act exits 0, which a naive comparison
// scores as a perfect match. The marker arrives on a line that is not JSON, so
// the skipped lines have to be SCANNED rather than dropped.
func TestUnsupportedPlatformIsFoundOnAPlainLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"a plain line", `[test/build] 🚧  Skipping unsupported platform -- Requested 'ubuntu-24.04'`},
		{"a JSON record's msg", `{"level":"warning","msg":"🚧  Skipping unsupported platform -- Requested 'ubuntu-24.04'","jobID":"build"}`},
		{"a malformed JSON line", `{"msg":"🚧  Skipping unsupported platform`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := newObservation()
			if err := parseStream(strings.NewReader(tc.line+"\n"), obs); err != nil {
				t.Fatal(err)
			}
			if !obs.sawUnsupportedPlatform {
				t.Fatal("the marker was not seen; act exits 0 here and the run scores as a match having done nothing")
			}
		})
	}

	t.Run("and a clean stream does not claim it", func(t *testing.T) {
		obs := newObservation()
		if err := parseStream(strings.NewReader(`{"jobID":"b","jobResult":"success"}`+"\n"), obs); err != nil {
			t.Fatal(err)
		}
		if obs.sawUnsupportedPlatform {
			t.Fatal("the marker fired on a clean stream")
		}
	})
}

// TestParseStreamFiltersByStageNotByName — join specific 1. `stage` is closed and
// typed, so dropping the injected steps is a FILTER, not a name match; Post
// records reuse the parent's stepID and would otherwise OVERWRITE the Main
// observation with a different conclusion.
func TestParseStreamFiltersByStageNotByName(t *testing.T) {
	stream := strings.Join([]string{
		`{"jobID":"b","msg":"⭐ Run Set up job","step":"Set up job","stepid":["--setup-job"]}`,
		`{"jobID":"b","stage":"Pre","stepID":["cache"],"step":"cache","stepResult":"success"}`,
		`{"jobID":"b","stage":"Main","stepID":["cache"],"step":"cache","stepResult":"success"}`,
		`{"jobID":"b","stage":"Post","stepID":["cache"],"step":"cache","stepResult":"failure"}`,
		`{"jobID":"b","msg":"⭐ Run Complete job","step":"Complete job","stepid":["--complete-job"]}`,
		`{"jobID":"b","jobResult":"success"}`,
	}, "\n")

	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, _ := obs.jobResults()
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if len(jobs[0].Steps) != 1 {
		t.Fatalf("got %d steps, want only the Main record: %+v", len(jobs[0].Steps), jobs[0].Steps)
	}
	if got := jobs[0].Steps[0].Conclusion; got != exec.ConclusionSuccess {
		t.Fatalf("the Post record overwrote the Main conclusion: %q", got)
	}
}

// TestParseStreamKeepsCompositeInnerStepsApart — join specific 3, and the
// ledger's recorded defect. One measured workflow yields three records — g, g.mk, g.1 — with NO declared
// order between them; keyed on the ordinal, three observations collapse onto one
// row and two overwrite each other.
func TestParseStreamKeepsCompositeInnerStepsApart(t *testing.T) {
	stream := strings.Join([]string{
		`{"jobID":"b","stage":"Main","stepID":["g","mk"],"step":"make","stepResult":"success"}`,
		`{"jobID":"b","stage":"Main","stepID":["g","1"],"step":"inner","stepResult":"failure"}`,
		`{"jobID":"b","stage":"Main","stepID":["g"],"step":"composite","stepResult":"failure"}`,
		`{"jobID":"b","jobResult":"failure"}`,
	}, "\n")

	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, _ := obs.jobResults()
	if len(jobs[0].Steps) != 3 {
		t.Fatalf("got %d steps, want 3 (g, g.mk, g.1): %+v", len(jobs[0].Steps), jobs[0].Steps)
	}
	seen := map[string]exec.Conclusion{}
	for _, s := range jobs[0].Steps {
		seen[exec.CanonicalStepID(s.StepID)] = s.Conclusion
		// A composite's inner step has no declared index.
		if len(s.StepID) > 1 && s.Ordinal != 0 {
			t.Errorf("inner step %v carries ordinal %d; it has no declared index", s.StepID, s.Ordinal)
		}
	}
	for _, want := range []string{"g", "g.mk", "g.1"} {
		if _, ok := seen[want]; !ok {
			t.Errorf("step %q is missing: %v", want, seen)
		}
	}
}

// TestDeclaredOrdinalIsNotReconstructed — join specific 2. act emits the declared
// `id:` when one was given, so for such a step there is no index in the stream
// at all. Numbering by order of appearance shifts every later step by one, with
// no error, the moment a declared step emits nothing.
func TestDeclaredOrdinalIsNotReconstructed(t *testing.T) {
	for _, tc := range []struct {
		id   []string
		want int
	}{
		{[]string{"0"}, 1},       // act's index is 0-based, Ordinal is 1-based
		{[]string{"3"}, 4},       //
		{[]string{"mid"}, 0},     // a declared id: carries no index
		{[]string{"g", "mk"}, 0}, // a composite inner step
		{[]string{"-1"}, 0},      // not a declaration index
		{nil, 0},
	} {
		if got := declaredOrdinal(tc.id); got != tc.want {
			t.Errorf("declaredOrdinal(%v) = %d, want %d", tc.id, got, tc.want)
		}
	}

	// Read through the stream: an id-bearing step must NOT be given the
	// position it happened to appear in.
	stream := strings.Join([]string{
		`{"jobID":"b","stage":"Main","stepID":["0"],"step":"first","stepResult":"success"}`,
		`{"jobID":"b","stage":"Main","stepID":["mid"],"step":"second","stepResult":"success"}`,
		`{"jobID":"b","stage":"Main","stepID":["2"],"step":"third","stepResult":"success"}`,
		`{"jobID":"b","jobResult":"success"}`,
	}, "\n")
	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, _ := obs.jobResults()
	got := []int{}
	for _, s := range jobs[0].Steps {
		got = append(got, s.Ordinal)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 0 || got[2] != 3 {
		t.Fatalf("ordinals = %v, want [1 0 3] — 0 means the channel carried no declared index, and reconstructing it would shift every later step", got)
	}
}

// TestMapResultNeverGuesses — cancelled / timed_out / action_required / neutral
// / stale have been observed at NO level, and act's mapping onto GitHub's enum
// has not been measured. A guessed mapping falls in the one forbidden
// direction: a false match.
func TestMapResultNeverGuesses(t *testing.T) {
	for _, fn := range []struct {
		name string
		f    func(string) exec.Conclusion
	}{
		{"mapStepResult", mapStepResult},
		{"mapJobResult", mapJobResult},
	} {
		t.Run(fn.name, func(t *testing.T) {
			for in, want := range map[string]exec.Conclusion{
				"success": exec.ConclusionSuccess,
				"failure": exec.ConclusionFailure,
				"skipped": exec.ConclusionSkipped,
				// Everything else is unmappable, never guessed.
				"cancelled":       exec.ConclusionUnmappable,
				"timed_out":       exec.ConclusionUnmappable,
				"neutral":         exec.ConclusionUnmappable,
				"action_required": exec.ConclusionUnmappable,
				"stale":           exec.ConclusionUnmappable,
				"":                exec.ConclusionUnmappable,
				"SUCCESS":         exec.ConclusionUnmappable,
			} {
				if got := fn.f(in); got != want {
					t.Errorf("%s(%q) = %q, want %q", fn.name, in, got, want)
				}
			}
		})
	}
}

// TestRanIsNotTheSameQuestionAsConcluded — a step act recorded as `skipped`
// produced a record and did not run; a step that is missing entirely produced
// neither. That difference is the whole reason --verbose is a requirement rather
// than a setting.
func TestRanIsNotTheSameQuestionAsConcluded(t *testing.T) {
	stream := strings.Join([]string{
		`{"jobID":"b","stage":"Main","stepID":["0"],"step":"build","stepResult":"failure"}`,
		`{"jobID":"b","stage":"Main","stepID":["1"],"step":"test","stepResult":"skipped"}`,
		`{"jobID":"b","jobResult":"failure"}`,
	}, "\n")
	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, _ := obs.jobResults()
	if !jobs[0].Steps[0].Ran {
		t.Error("a failed step is recorded as not having run")
	}
	if jobs[0].Steps[1].Ran {
		t.Error("a skipped step is recorded as having run")
	}
	if jobs[0].Steps[1].Conclusion != exec.ConclusionSkipped {
		t.Errorf("the skipped step lost its conclusion: %q", jobs[0].Steps[1].Conclusion)
	}
}

// TestJobWithoutResultIsReportedNotInvented — U-7's "never skip silently". A job
// that left step records and never a job result is harness_error /
// job_without_result, and folding it in with a guessed conclusion is a silent
// failure that looks like data.
func TestJobWithoutResultIsReportedNotInvented(t *testing.T) {
	stream := strings.Join([]string{
		`{"jobID":"done","stage":"Main","stepID":["0"],"step":"a","stepResult":"success"}`,
		`{"jobID":"done","jobResult":"success"}`,
		`{"jobID":"lost","stage":"Main","stepID":["0"],"step":"b","stepResult":"success"}`,
	}, "\n")
	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, without := obs.jobResults()
	if len(jobs) != 1 || jobs[0].JobID != "done" {
		t.Fatalf("jobs = %+v, want only `done`", jobs)
	}
	if len(without) != 1 || without[0] != "lost" {
		t.Fatalf("jobsWithoutResult = %v, want [lost]", without)
	}
}

// TestMatrixAbsentIsNotMatrixEmpty — act's cross-leg result bleed, through
// the parser. For act 1 is
// EXPECTED and 0 is an EVENT, and a job with no matrix must not collapse into
// the same key as a job whose leg could not be told apart.
func TestMatrixAbsentIsNotMatrixEmpty(t *testing.T) {
	stream := strings.Join([]string{
		`{"jobID":"b","matrix":{},"jobResult":"success"}`,
		`{"jobID":"b","jobResult":"failure"}`,
	}, "\n")
	obs := newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, _ := obs.jobResults()
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2 — an emitted empty matrix and no matrix at all are different facts", len(jobs))
	}
	if !jobs[0].MatrixLeg.Resolved() {
		t.Error("the record carrying `matrix:{}` came back unresolved")
	}
	if jobs[1].MatrixLeg.Resolved() {
		t.Error("the record carrying no matrix came back resolved")
	}

	// Two typed legs of one job stay apart.
	stream = strings.Join([]string{
		`{"jobID":"b","matrix":{"node":20},"jobResult":"success"}`,
		`{"jobID":"b","matrix":{"node":"20"},"jobResult":"failure"}`,
	}, "\n")
	obs = newObservation()
	if err := parseStream(strings.NewReader(stream), obs); err != nil {
		t.Fatal(err)
	}
	jobs, _ = obs.jobResults()
	if len(jobs) != 2 {
		t.Fatalf("the typed and string matrix legs collapsed into %d job(s)", len(jobs))
	}
}
