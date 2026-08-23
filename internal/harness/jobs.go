package harness

import (
	"sort"

	"github.com/fatihy101/standpipe/internal/exec"
)

// (a) and (b) — the plan comparison and the model's `skipped` mapping.

// observedJobs indexes the engine's Result for the join.
//
// The two maps are not one map with a nil-able key on purpose. An observation
// whose leg was NOT resolved is a different fact from an observation of a job
// with no matrix, and collapsing them makes a matrix job whose legs the engine
// could not tell apart look like a single legless job — which then matches
// exactly one model leg and silently loses the rest.
type observedJobs struct {
	byKey      map[JobKey]*exec.JobResult
	byJobID    map[string][]*exec.JobResult // leg-UNRESOLVED observations only
	allByJobID map[string][]*exec.JobResult
}

func indexObserved(jobs []exec.JobResult) *observedJobs {
	o := &observedJobs{
		byKey:      map[JobKey]*exec.JobResult{},
		byJobID:    map[string][]*exec.JobResult{},
		allByJobID: map[string][]*exec.JobResult{},
	}
	for i := range jobs {
		j := &jobs[i]
		o.allByJobID[j.JobID] = append(o.allByJobID[j.JobID], j)
		if j.MatrixLeg.Resolved() {
			o.byKey[JobKey{JobID: j.JobID, Leg: j.MatrixLeg.String()}] = j
			continue
		}
		o.byJobID[j.JobID] = append(o.byJobID[j.JobID], j)
	}
	return o
}

// find returns the observation for a model job, and whether the match was made
// at leg granularity.
//
// The plan comparison was corrected by measuring the engine's JSON log: legs
// ARE distinguishable — every
// record carries a TYPED matrix object — so `A ⊆ P` is established at LEG
// granularity and joins straight onto the model's leg_key. The unresolved path
// below is the fallback for the one shape the measurement showed does NOT
// carry a matrix: the debug-level `{"jobID":"extra","jobResult":"skipped"}`
// record that (b) uses as a cross-check.
func (o *observedJobs) find(j ModelJob) (obs *exec.JobResult, byLeg bool) {
	if r, ok := o.byKey[j.Key()]; ok {
		return r, true
	}
	if rs := o.byJobID[j.JobID]; len(rs) > 0 {
		return rs[0], false
	}
	return nil, false
}

// checkPlanAgainstGitHub is row 5 — the plan comparison's first assertion,
// P ≡ G.
//
// THIS IS THE ONE ASSERTION IN THE DESIGN THAT IS ABOUT US, NOT ABOUT THE
// ENGINE. If the model does not predict GitHub's job set exactly, our model is
// wrong or something we did not model is in play, and nothing downstream can
// be trusted. It is also the only self-testing part of this design.
func (v *validation) checkPlanAgainstGitHub() bool {
	g := v.in.GitHub

	for _, j := range g.Jobs {
		if !j.keyed() {
			// Row 5 cannot be EVALUATED, which is not the same as row 5
			// failing. GitHub exposes a display name and the rendering rule
			// that turns one into (job_id, leg_key) has not been measured, so
			// the adapter said it could not resolve it. That is row 7's fact
			// and gets row 7's reason. Both rows produce `unmeasurable` and
			// both cost the same 5% budget, so the ordering has no
			// arithmetic consequence here — only the counter differs, and the
			// counter should name the truth.
			v.finding(Finding{Part: "a", Kind: FindingJobIdentityUnresolved, Job: j.Name,
				Detail: "GitHub job identity was not resolved to (job_id, leg_key); P ≡ G cannot be evaluated"})
			v.fail(7, exec.OutcomeUnmeasurable, exec.RefusalJobIdentityUnmapped)
			return false
		}
	}

	if len(g.Jobs) == 0 {
		// The plan comparison's safety valve. `on.push.branches`/`paths` filters are outside P
		// deliberately — the engine does not apply them at all [S — Run D], so
		// it produces a conclusion for a run GitHub never created. That is the
		// pre-filter's problem, not the validator's; but a run with no GitHub
		// side has nothing to be compared against and must not be scored.
		v.finding(Finding{Part: "a", Kind: FindingGitHubRunEmpty,
			Detail: "GitHub produced no jobs for this run — there is no side to compare against"})
		v.fail(5, exec.OutcomeUnmeasurable, exec.RefusalPlanDisagreesWithGitHub)
		return false
	}

	inP := map[JobKey]bool{}
	for _, j := range v.in.Model.Jobs {
		inP[j.Key()] = true
	}
	inG := map[JobKey]bool{}
	for _, j := range g.Jobs {
		inG[j.Key] = true
	}

	var missing, extra []string
	for k := range inP {
		if !inG[k] {
			missing = append(missing, k.String())
		}
	}
	for k := range inG {
		if !inP[k] {
			extra = append(extra, k.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) == 0 && len(extra) == 0 {
		return true
	}
	for _, k := range missing {
		v.finding(Finding{Part: "a", Kind: FindingPlanJobNotInGitHub, Job: k,
			Detail: "the model predicted this job and GitHub did not run it"})
	}
	for _, k := range extra {
		v.finding(Finding{Part: "a", Kind: FindingGitHubJobNotInPlan, Job: k,
			Detail: "GitHub ran this job and the model did not predict it"})
	}
	v.fail(5, exec.OutcomeUnmeasurable, exec.RefusalPlanDisagreesWithGitHub)
	return false
}

// checkObservedAgainstPlan is row 6 — the plan comparison's second
// assertion: A ⊆ P at LEG
// granularity, and every job the model does not call `skipped` has a result.
func (v *validation) checkObservedAgainstPlan() bool {
	inP := map[JobKey]bool{}
	jobIDsInP := map[string]bool{}
	for _, j := range v.in.Model.Jobs {
		inP[j.Key()] = true
		jobIDsInP[j.JobID] = true
	}

	ok := true
	for k := range v.obs.byKey {
		if !inP[k] {
			v.finding(Finding{Part: "a", Kind: FindingObservedJobNotInPlan, Job: k.String(),
				Detail: "the engine produced a result for a job the model did not plan"})
			ok = false
		}
	}
	for id := range v.obs.byJobID {
		if !jobIDsInP[id] {
			v.finding(Finding{Part: "a", Kind: FindingObservedJobNotInPlan, Job: id,
				Detail: "the engine produced a leg-unresolved result for a job the model did not plan"})
			ok = false
		}
	}

	for _, j := range v.in.Model.Jobs {
		if j.Skipped() {
			// Absence from A is EXPECTED behaviour for a job the model
			// calls skipped, and is not a defect.
			continue
		}
		if obs, _ := v.obs.find(j); obs == nil {
			v.finding(Finding{Part: "a", Kind: FindingJobWithoutResult, Job: j.Key().String(),
				Detail: "the model expects this job to run and the engine produced no result for it"})
			ok = false
		}
	}

	if !ok {
		v.fail(6, exec.OutcomeHarnessError, exec.RefusalJobWithoutResult)
	}
	return ok
}

// checkModelMapping is row 7 — (b), the model's `skipped` mapping.
//
// It also builds the RECONCILED job set the derivation runs over. That set is
// (b)'s actual product: a job the model calls `skipped` and the engine
// legitimately never mentioned still has to be in the set, or a workflow whose
// jobs are all `if:`-false reaches the derivation with an empty job list.
func (v *validation) checkModelMapping() bool {
	ok := true
	reconciled := make([]exec.JobResult, 0, len(v.in.Model.Jobs))

	for _, j := range v.in.Model.Jobs {
		if j.Unevaluable != "" {
			// The mapping's limit. The model may only call a job `skipped` when its
			// `if:` was EVALUABLE — otherwise an empty payload read through
			// `github.event.*` gets confirmed as "skipped" BY THE MODEL and
			// the g11 class comes back from inside the validator.
			//
			// The design states the verdict (`unmeasurable`) and does not name
			// a reason. This is row 7 and takes row 7's reason; the true cause
			// is written into the finding so the counter is not the only
			// record. This is an interpretation, not a stated rule.
			v.finding(Finding{Part: "b", Kind: FindingGuardUnevaluable, Job: j.Key().String(),
				Detail: "the model could not evaluate this job's guard: " + j.Unevaluable})
			ok = false
			continue
		}

		obs, byLeg := v.obs.find(j)

		if j.Skipped() {
			if obs == nil {
				// The measured default-level behaviour: no line, no result
				// [M — q19 b05, b06]. Not a defect. The expectation comes from
				// the model and the job enters the derivation as skipped.
				reconciled = append(reconciled, exec.JobResult{
					JobID: j.JobID, MatrixLeg: j.Leg,
					Conclusion: exec.ConclusionSkipped,
					Steps:      []exec.StepObservation{},
				})
				continue
			}
			// The CROSS-CHECK, and it only works in one direction. At
			// debug level the engine does emit {"jobID":..,"jobResult":
			// "skipped"} [M]. A MISSING record is not a defect; a
			// CONTRADICTING one is.
			if obs.Conclusion == exec.ConclusionSkipped {
				v.finding(Finding{Part: "b", Kind: FindingSkipConfirmed, Job: j.Key().String(),
					Detail: "the engine's own record agrees the job was skipped"})
			} else {
				v.finding(Finding{Part: "b", Kind: FindingSkipContradicted, Job: j.Key().String(),
					Detail: "the model calls this job skipped and the engine recorded " + string(obs.Conclusion)})
				ok = false
				continue
			}
			reconciled = append(reconciled, *obs)
			continue
		}

		if obs == nil {
			// Row 6 already failed for this; guard so the reconciled set is
			// never built from a hole.
			ok = false
			continue
		}
		if !byLeg && j.HasMatrix {
			// The engine gave a leg-unresolved result for a job that HAS legs.
			// Mapping it onto one of them would be a guess, and a guessed leg
			// pairs the wrong rows with no error at all.
			if len(v.obs.allByJobID[j.JobID]) != 1 || len(v.legsOf(j.JobID)) != 1 {
				v.finding(Finding{Part: "b", Kind: FindingLegUnresolved, Job: j.Key().String(),
					Detail: "the engine could not tell this job's matrix legs apart and the model declares more than one"})
				ok = false
				continue
			}
		}
		reconciled = append(reconciled, *obs)
	}

	if !ok {
		// Both of row 7's failure shapes carry row 7's reason. The
		// CONTRADICTED cross-check is the exception and the design names its reason
		// explicitly: plan_disagrees_with_github.
		reason := exec.RefusalJobIdentityUnmapped
		for _, f := range v.verdict.Findings {
			if f.Kind == FindingSkipContradicted {
				reason = exec.RefusalPlanDisagreesWithGitHub
				break
			}
		}
		v.fail(7, exec.OutcomeUnmeasurable, reason)
		return false
	}

	v.reconciled = reconciled
	return true
}

func (v *validation) legsOf(jobID string) []JobKey {
	var out []JobKey
	for _, j := range v.in.Model.Jobs {
		if j.JobID == jobID {
			out = append(out, j.Key())
		}
	}
	return out
}
