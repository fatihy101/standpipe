package harness

import (
	"sort"
	"strconv"

	"github.com/fatihy101/standpipe/internal/exec"
)

// (c) — step count and step identity. This is the part the validator was
// REWRITTEN for.
//
// Job conclusions cannot tell "ran and passed" apart from "silently omitted",
// and g11 is the case: a job whose expensive steps are guarded on
// `if: github.event.*` loses them under the engine while both sides conclude
// `success`. The original validator passed that run and the engine had done ZERO of the
// work. That is a silent SUCCESS, scored by the gate as a
// match.
//
// THE JOIN KEY IS (jobID, matrix, stage=="Main", stepID) [M]. Dropping
// the injected steps is a FILTER and not a name match: `Set up job` and
// `Complete job` carry neither `stage` nor `stepID`, and `Post` records reuse
// the parent's stepID with stage "Post" — so stage is part of the key, not a
// pre-filter that can be dropped. The engine's parser has already applied that
// filter; what is left for this file is the MODEL side of the same key, which
// it computes ITSELF (DeclaredStepID) rather than reading back off the engine.
//
// GitHub's steps[].number IS NOT A JOIN KEY. It is not a dense declared
// ordinal — in g11 the declared steps are numbered 2,3,4,5 and then jump to
// 10,11 [V]. Keying off it matches the wrong steps and reports no error.

func (v *validation) checkSteps() bool {
	ghJobs := map[JobKey]*GitHubJob{}
	if v.in.GitHub != nil {
		for i := range v.in.GitHub.Jobs {
			j := &v.in.GitHub.Jobs[i]
			if j.keyed() {
				ghJobs[j.Key] = j
			}
		}
	}

	unavailable := false
	for _, mj := range v.in.Model.Jobs {
		if mj.Skipped() {
			// Neither side runs steps for a skipped job. Comparing empty sets
			// is not a check.
			continue
		}
		obs, _ := v.obs.find(mj)
		if obs == nil {
			continue // row 6's problem, already reported
		}
		if v.checkJobSteps(mj, obs, ghJobs[mj.Key()]) {
			unavailable = true
		}
	}

	if unavailable {
		// Row 8's FIRST product: the step set could not be READ. That is a
		// measurement that was not made — the denominator's problem.
		//
		// It is kept EXCEPTIONAL on purpose. The measurement of the channel
		// did NOT show that a record is produced on every run, and it is
		// still an open question whether a declared step can produce no record at any level.
		// (c)'s soundness depends on that class being empty, so this reason
		// fires only when a job that RAN produced NO declared step record at
		// all — a partial miss is a MISMATCH, which is g11 under the default
		// log level.
		v.fail(8, exec.OutcomeHarnessError, exec.RefusalStepSetUnavailable)
		return false
	}
	// Row 8's SECOND product is a MISMATCH, and it does NOT stop the run:
	// The verdict order says the two must not be confused — the first is the denominator's
	// problem, the second is the numerator's. A step mismatch under matching
	// run conclusions is exactly g11 and is scored as a mismatch at row 10.
	return true
}

// checkJobSteps returns true when the step set was UNREADABLE for this job.
func (v *validation) checkJobSteps(mj ModelJob, obs *exec.JobResult, gh *GitHubJob) bool {
	job := mj.Key().String()

	declared := map[string]*exec.StepObservation{}
	var inner []exec.StepObservation
	for i := range obs.Steps {
		s := &obs.Steps[i]
		if len(s.StepID) == 0 {
			continue
		}
		if len(s.StepID) > 1 {
			// A composite action's INNER step. It is machine
			// readable and it is recorded, but it is NOT a declared step and
			// must not be counted as one: GitHub's job exposes the step that
			// USES the composite and not what is inside it, so counting inner
			// records would over-count against GitHub on every composite.
			//
			// The step-count assertion says "the count of stage==Main
			// records" literally; the inner-step rule gives the discriminator
			// that makes the literal reading wrong. This is an
			// interpretation, not a stated rule.
			inner = append(inner, *s)
			continue
		}
		declared[exec.CanonicalStepID(s.StepID)] = s
	}

	for _, s := range inner {
		v.finding(Finding{Part: "c", Kind: FindingCompositeInner, Job: job, Step: exec.CanonicalStepID(s.StepID),
			Detail: "inner step of a composite action; recorded, not counted as declared"})
	}

	if len(mj.Steps) > 0 && len(declared) == 0 && obs.Conclusion != exec.ConclusionSkipped {
		v.finding(Finding{Part: "c", Kind: FindingStepSetUnavailable, Job: job,
			Detail: "the job ran and produced no declared step record at all, while the model declares " + strconv.Itoa(len(mj.Steps))})
		return true
	}

	// ASSERTION 1 — COUNT. Run against both sides that can supply one.
	if gh != nil && gh.DeclaredStepCount >= 0 && gh.DeclaredStepCount != len(declared) {
		v.mismatch(Finding{Part: "c", Kind: FindingStepCount, Job: job,
			Detail: "GitHub ran " + strconv.Itoa(gh.DeclaredStepCount) + " declared steps; the engine recorded " + strconv.Itoa(len(declared))})
	}
	if gh != nil && gh.DeclaredStepCount < 0 {
		v.finding(Finding{Part: "c", Kind: FindingStepCountUnknown, Job: job,
			Detail: "GitHub's declared step count is unknown; the count assertion degraded to model-vs-engine"})
	}
	if len(mj.Steps) != len(declared) {
		v.mismatch(Finding{Part: "c", Kind: FindingStepCount, Job: job,
			Detail: "the model declares " + strconv.Itoa(len(mj.Steps)) + " steps; the engine recorded " + strconv.Itoa(len(declared))})
	}

	// ASSERTION 2 — IDENTITY. Count alone is not enough: one step vanishing
	// while another runs an extra time preserves the count.
	ghByIndex, ghByName := indexGitHubSteps(gh)
	matched := map[string]bool{}

	for i, ms := range mj.Steps {
		key := mj.StepKey(i)
		as, ok := declared[key]
		if !ok {
			v.mismatch(Finding{Part: "c", Kind: FindingStepMissing, Job: job, Step: key,
				Detail: "the model declares this step and the engine recorded no record for it"})
			continue
		}
		matched[key] = true

		if ms.Name != "" && as.Name != "" && as.Name != ms.Name {
			v.mismatch(Finding{Part: "c", Kind: FindingStepName, Job: job, Step: key,
				Detail: "the model declares name " + strconv.Quote(ms.Name) + "; the engine recorded " + strconv.Quote(as.Name)})
		}

		// DETAIL 4, and it is applied BEFORE any comparison. The engine writes
		// an OUTCOME; GitHub publishes a CONCLUSION. For a step with
		// continue-on-error: true that FAILED, the engine says failure and
		// GitHub says success [M + S + V]. Neither side gives both. Skipping
		// this produces a FALSE MISMATCH on every such step — a safe direction,
		// but one the gate would have to explain forever.
		adjusted := as.Conclusion
		if ms.ContinueOnError && adjusted == exec.ConclusionFailure {
			adjusted = exec.ConclusionSuccess
			v.finding(Finding{Part: "c", Kind: FindingContinueOnError, Job: job, Step: key,
				Detail: "the model's continue-on-error turned the engine's failure into success before comparison"})
		}
		v.stepConclusions = append(v.stepConclusions, stepConclusion{job: job, step: key, value: as.Conclusion})

		if gh == nil {
			continue // an outage run; there is no GitHub side, by construction
		}
		gs := lookupGitHubStep(ghByIndex, ghByName, i, ms.Name)
		if gs == nil {
			// Recorded rather than passed over. The GitHub adapter could not
			// place this record, so the per-step conclusion comparison is not
			// made for it — but the count assertion above still applies, and
			// this is never silence.
			v.finding(Finding{Part: "c", Kind: FindingStepUnjoined, Job: job, Step: key,
				Detail: "no GitHub step record could be joined to this declared step; its conclusion was not compared"})
			continue
		}
		if adjusted != gs.Conclusion {
			v.mismatch(Finding{Part: "c", Kind: FindingStepConclusion, Job: job, Step: key,
				Detail: "GitHub concluded " + string(gs.Conclusion) + "; the engine observed " + string(adjusted)})
		}
	}

	var extra []string
	for key := range declared {
		if !matched[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		v.mismatch(Finding{Part: "c", Kind: FindingStepExtra, Job: job, Step: key,
			Detail: "the engine recorded a declared step the model does not declare"})
	}
	return false
}

func indexGitHubSteps(gh *GitHubJob) (byIndex map[int]*GitHubStep, byName map[string][]*GitHubStep) {
	byIndex = map[int]*GitHubStep{}
	byName = map[string][]*GitHubStep{}
	if gh == nil {
		return byIndex, byName
	}
	for i := range gh.Steps {
		s := &gh.Steps[i]
		if s.DeclaredIndex >= 0 {
			byIndex[s.DeclaredIndex] = s
		}
		if s.Name != "" {
			byName[s.Name] = append(byName[s.Name], s)
		}
	}
	return byIndex, byName
}

// lookupGitHubStep places a GitHub step record against a declared step.
//
// Two ways, in order, and NO third:
//
//  1. the adapter's DeclaredIndex, when it supplied one;
//  2. an exact, UNIQUE match on the declared `name:`.
//
// There is deliberately no fallback to position: GitHub's own numbering is not
// dense, so ordering its records and pairing by position is the
// same defect as keying off steps[].number. An unplaceable record is reported,
// not guessed.
func lookupGitHubStep(byIndex map[int]*GitHubStep, byName map[string][]*GitHubStep, index int, name string) *GitHubStep {
	if s, ok := byIndex[index]; ok {
		return s
	}
	if name == "" {
		return nil
	}
	if ss := byName[name]; len(ss) == 1 {
		return ss[0]
	}
	return nil
}
