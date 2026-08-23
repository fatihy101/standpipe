package act

import (
	"bufio"
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/fatihy101/standpipe/internal/exec"
)

// Parsing `act --json --verbose` into a job and step set (U-4; the
// validator's step-level channel).
//
// The channel exists, is structured, and was MEASURED [M]. Each record
// carries jobID, matrix, stage, stepID, step and stepResult. The stepResult
// vocabulary is closed — success, failure, skipped — and that closedness comes
// from pkg/model/step_result.go [S], not from the three having been observed;
// the two markers stay side by side.
//
// FOUR JOIN SPECIFICS. Each one, implemented wrong, produces a validator
// that is SILENTLY wrong.
//
//  1. The join key is (jobID, matrix, stage=="Main", stepID). `stage` is closed
//     and typed, so dropping the injected steps is a FILTER, not a name match:
//     `Set up job` and `Complete job` carry neither stage nor stepID. `Post`
//     steps fall out by themselves.
//  2. stepID is NOT always an ordinal. act emits the declared `id:` when one
//     was given and the declared index only when one was not [M;
//     S job_executor.go:73]. The model has to reproduce that rule on its own
//     side. And GitHub's steps[].number cannot be the key either: it is not a
//     dense declaration order — in one measured workflow the declared steps are numbered 2,3,4,5
//     and then jump to 10,11 [V].
//  3. A composite action's inner steps are separated by len(stepID) > 1:
//     ["g"] is declared, ["g","mk"] is inside it.
//  4. act writes an OUTCOME; GitHub publishes a CONCLUSION. For a step with
//     continue-on-error: true that FAILED, act says failure and GitHub says
//     success. Neither side gives both. continue-on-error is applied FROM THE
//     MODEL, by the validator, BEFORE comparison — not here. Applying it here
//     would mean reporting a conclusion that was never observed.
//
// TWO MEASURED PROPERTIES OF THE STREAM THAT THE DESIGN DOES NOT STATE, both
// confirmed against the first measurement's own logs on 2026-08-22:
//
//   - The injected pseudo-steps carry a LOWERCASE `stepid`, not `stepID`
//     (values ["--setup-job"] and ["--complete-job"]). Go's encoding/json
//     matches field names CASE-INSENSITIVELY, so a struct tagged `stepID` also
//     absorbs `stepid` and the injected records acquire a step identity they do
//     not have. Records are therefore decoded through exact map keys.
//   - act writes plain, non-JSON lines to the stream even under --json — e.g.
//     `Error: Job 'shard' failed`. A streaming json.Decoder stops at the first
//     one and everything after it is lost. Non-JSON lines are skipped, and
//     scanned for the unsupported-platform marker.

// unsupportedPlatformMarker is verdict step 4's hard harness error, and the
// part of the validator's original design that did not change. No job runs, no result is written, and the process
// exits 0 [M/S — run_context.go:817-823] — which a naive comparison
// scores as a perfect match.
const unsupportedPlatformMarker = "Skipping unsupported platform"

// stepResult / jobResult values act is known to emit.
const (
	actSuccess = "success"
	actFailure = "failure"
	actSkipped = "skipped"
)

type record struct {
	level      string
	msg        string
	jobID      string
	hasMatrix  bool
	matrix     map[string]json.RawMessage
	stage      string
	stepID     []string
	step       string
	stepResult string
	jobResult  string
	hasJobRes  bool
}

type observation struct {
	// jobs preserves first-seen order so the result is deterministic.
	order []jobKey
	jobs  map[jobKey]*jobAccum

	sawUnsupportedPlatform bool
	// nonJSONLines is kept for the record; act's own plain-text lines are the
	// only ones seen under measurement.
	nonJSONLines int
}

type jobKey struct {
	jobID string
	leg   string
	// legResolved distinguishes "no matrix object was emitted" from "the
	// matrix object was empty". Without it a job with no matrix and a job whose
	// leg could not be told apart collapse into one key.
	legResolved bool
}

type jobAccum struct {
	key       jobKey
	matrix    map[string]json.RawMessage
	hasResult bool
	result    string
	stepOrder []string
	steps     map[string]*exec.StepObservation
}

// parseStream reads one of act's output streams and folds it into obs.
// Both stdout and stderr are fed through it: which stream logrus writes to is
// not something this package should depend on.
func parseStream(r io.Reader, obs *observation) error {
	sc := bufio.NewScanner(r)
	// act echoes a step's raw output in `raw_output` records, and a single
	// line of build output can be long.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			obs.nonJSONLines++
			if strings.Contains(line, unsupportedPlatformMarker) {
				obs.sawUnsupportedPlatform = true
			}
			continue
		}
		rec, ok := decodeRecord([]byte(line))
		if !ok {
			obs.nonJSONLines++
			if strings.Contains(line, unsupportedPlatformMarker) {
				obs.sawUnsupportedPlatform = true
			}
			continue
		}
		obs.absorb(rec)
	}
	return sc.Err()
}

// decodeRecord decodes one log line through EXACT map keys.
//
// Not a struct: Go's encoding/json is case-insensitive on field names, and act
// emits both `stepID` (declared steps, with a stage) and `stepid` (the injected
// Set up job / Complete job pseudo-steps, with no stage). A tagged struct would
// merge them and hand the injected records a step identity.
func decodeRecord(line []byte) (record, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return record{}, false
	}
	var r record
	r.level = str(raw, "level")
	r.msg = str(raw, "msg")
	r.jobID = str(raw, "jobID")
	r.stage = str(raw, "stage")
	r.step = str(raw, "step")
	r.stepResult = str(raw, "stepResult")
	if v, ok := raw["jobResult"]; ok {
		_ = json.Unmarshal(v, &r.jobResult)
		r.hasJobRes = true
	}
	if v, ok := raw["matrix"]; ok {
		r.hasMatrix = true
		r.matrix = map[string]json.RawMessage{}
		if err := json.Unmarshal(v, &r.matrix); err != nil {
			// A matrix that will not decode is not silently treated as absent:
			// leaving hasMatrix true with a nil map keeps it distinguishable
			// from "no matrix emitted".
			r.matrix = nil
		}
	}
	// Only the exact `stepID` spelling counts as a step identity.
	if v, ok := raw["stepID"]; ok {
		_ = json.Unmarshal(v, &r.stepID)
	}
	return r, true
}

func str(m map[string]json.RawMessage, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

func newObservation() *observation {
	return &observation{jobs: map[jobKey]*jobAccum{}}
}

func (o *observation) absorb(r record) {
	if strings.Contains(r.msg, unsupportedPlatformMarker) {
		o.sawUnsupportedPlatform = true
	}
	if r.jobID == "" {
		return // act's own start-up lines carry no job
	}

	leg := exec.CanonicalLegKey(r.matrix)
	key := jobKey{jobID: r.jobID, leg: leg.String(), legResolved: r.hasMatrix && r.matrix != nil}
	acc, ok := o.jobs[key]
	if !ok {
		acc = &jobAccum{key: key, matrix: r.matrix, steps: map[string]*exec.StepObservation{}}
		o.jobs[key] = acc
		o.order = append(o.order, key)
	}

	if r.hasJobRes {
		acc.hasResult = true
		acc.result = r.jobResult
		return
	}

	// Specific 1: the filter. Only stage=="Main" records are DECLARED steps.
	// Pre and Post fall out here, and so do the injected pseudo-steps, which
	// carry no stage at all.
	if r.stage != "Main" || len(r.stepID) == 0 {
		return
	}
	id := exec.CanonicalStepID(r.stepID)
	st, ok := acc.steps[id]
	if !ok {
		st = &exec.StepObservation{
			StepID:  append([]string(nil), r.stepID...),
			Ordinal: declaredOrdinal(r.stepID),
		}
		acc.steps[id] = st
		acc.stepOrder = append(acc.stepOrder, id)
	}
	if r.step != "" {
		st.Name = r.step
	}
	if r.stepResult != "" {
		st.Conclusion = mapStepResult(r.stepResult)
		// Specific: "ran" is not the same question as "concluded". A step act
		// recorded as skipped produced a record and did not run; a step that is
		// missing entirely produced neither, and that difference is the whole
		// reason --verbose is a requirement rather than a setting.
		st.Ran = r.stepResult != actSkipped
	}
}

// declaredOrdinal recovers the 1-based declared index WHEN THE CHANNEL
// CARRIES IT, and returns 0 when it does not.
//
// Specific 2: act emits the declared `id:` if the step had one, so for such a
// step there is no index in the stream at all. The alternative — numbering by
// order of appearance — is refused: if a declared step ever emits nothing, that
// numbering shifts every later step by one with no error at all, and whether
// such a step can exist is still an open question. StepID is the join
// key; Ordinal is a convenience that says so when it does not know.
//
// A composite's inner step (len > 1) has no declared index either.
func declaredOrdinal(stepID []string) int {
	if len(stepID) != 1 {
		return 0
	}
	n, err := strconv.Atoi(stepID[0])
	if err != nil || n < 0 {
		return 0
	}
	return n + 1 // act's index is 0-based; Ordinal is 1-based
}

// mapStepResult maps act's step outcome onto the vocabulary.
//
// Anything outside the three measured values becomes ConclusionUnmappable. It
// is NEVER guessed: act's mapping onto GitHub's enum has not been measured, and
// cancelled / timed_out / action_required / neutral / stale have been observed
// at NO level. A guessed mapping falls in the one forbidden direction: a false
// match.
func mapStepResult(v string) exec.Conclusion {
	switch v {
	case actSuccess:
		return exec.ConclusionSuccess
	case actFailure:
		return exec.ConclusionFailure
	case actSkipped:
		return exec.ConclusionSkipped
	default:
		return exec.ConclusionUnmappable
	}
}

func mapJobResult(v string) exec.Conclusion {
	switch v {
	case actSuccess:
		return exec.ConclusionSuccess
	case actFailure:
		return exec.ConclusionFailure
	case actSkipped:
		return exec.ConclusionSkipped
	default:
		return exec.ConclusionUnmappable
	}
}

// jobResults renders the accumulated observation as the interface's type.
//
// jobsWithoutResult is returned separately rather than folded in: a job that
// left step records but never a job result is U-7's "never skip silently", and
// the caller turns it into harness_error / job_without_result.
func (o *observation) jobResults() (jobs []exec.JobResult, jobsWithoutResult []string) {
	for _, key := range o.order {
		acc := o.jobs[key]
		steps := make([]exec.StepObservation, 0, len(acc.stepOrder))
		for _, id := range acc.stepOrder {
			steps = append(steps, *acc.steps[id])
		}
		if !acc.hasResult {
			jobsWithoutResult = append(jobsWithoutResult, acc.key.jobID)
			continue
		}
		jr := exec.JobResult{
			JobID:      acc.key.jobID,
			Conclusion: mapJobResult(acc.result),
			// Never nil (U-4). An empty slice means "no step ran"; not knowing
			// is a refusal, not an empty slice.
			Steps: steps,
		}
		if acc.key.legResolved {
			jr.MatrixLeg = exec.CanonicalLegKey(acc.matrix)
		}
		jobs = append(jobs, jr)
	}
	return jobs, jobsWithoutResult
}
