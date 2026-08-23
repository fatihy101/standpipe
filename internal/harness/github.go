package harness

import (
	"fmt"

	"github.com/fatihy101/standpipe/internal/exec"
)

// GitHubRun is set G: what GitHub actually produced for the same
// commit and the same workflow.
//
// It is an INPUT to this package and is filled by whatever reads the GitHub
// API. This package does not talk to GitHub, and it does not parse GitHub's
// display names.
type GitHubRun struct {
	RunID int64

	// Conclusion is the run-level conclusion GitHub published — the unit the
	// gate is scored in. It must be one of GitHub's eight; `unmappable` is
	// this design's own value and can never come from GitHub.
	Conclusion exec.Conclusion

	Jobs []GitHubJob
}

// GitHubJob is one job GitHub ran.
type GitHubJob struct {
	// Key is the (job_id, leg_key) this job corresponds to.
	//
	// AN EMPTY JobID MEANS THE IDENTITY WAS NOT RESOLVED, and that is a legal
	// value that produces row 7's `unmeasurable / job_identity_unmapped`.
	// It is legal on purpose: GitHub's API exposes a DISPLAY NAME
	// ("build (ubuntu-latest, 1.21)"), not a job id and a leg key, and turning
	// one into the other is a rendering rule this design has not measured.
	// Guessing it here would pair the wrong legs with no error at all — the
	// same class as keying off GitHub's steps[].number. The GitHub adapter resolves
	// it or says it could not.
	Key JobKey

	// Name is GitHub's display name, kept for diagnostics only. It is never a
	// join key.
	Name string

	Conclusion exec.Conclusion

	// DeclaredStepCount is how many DECLARED steps GitHub ran for this job, or
	// -1 when the adapter could not separate declared steps from the injected
	// and Post ones.
	//
	// -1 is a measurement outcome and not a default. The step-count assertion
	// compares
	// GitHub's declared step count against the engine's; when GitHub's side is
	// unknown the assertion degrades to model-vs-engine, which still catches
	// the g11 class, and the degradation is RECORDED rather than assumed.
	DeclaredStepCount int

	Steps []GitHubStep
}

// GitHubStep is one step record from GitHub's job.
type GitHubStep struct {
	Name string

	// Number is GitHub's own `steps[].number`. It is RECORDED and is NEVER a
	// join key: it is not a dense declaration order — in g11
	// the declared steps are numbered 2,3,4,5 and then jump to 10,11 [V]. An
	// implementation that keys off it matches the wrong steps and reports no
	// error.
	Number int

	// DeclaredIndex is the 0-based DECLARED index this record corresponds to,
	// or -1 when the adapter could not tell. See GitHubJob.Key for why an
	// unknown is carried rather than derived.
	DeclaredIndex int

	Conclusion exec.Conclusion
}

// keyed reports whether the job's identity was resolved at all.
func (g GitHubJob) keyed() bool { return g.Key.JobID != "" }

// Validate rejects a GitHub side that cannot be the right-hand side of an
// assertion. It does NOT reject unresolved identities — those are a verdict
// (row 7), not a caller error.
func (r GitHubRun) Validate() error {
	if !r.Conclusion.IsGitHub() {
		return fmt.Errorf("harness: GitHubRun.Conclusion %q is not one of GitHub's eight — `unmappable` is this design's value and cannot come from GitHub", r.Conclusion)
	}
	seen := map[JobKey]bool{}
	for i, j := range r.Jobs {
		if !j.Conclusion.IsGitHub() {
			return fmt.Errorf("harness: GitHubRun.Jobs[%d] (%s) conclusion %q is not one of GitHub's eight", i, j.Name, j.Conclusion)
		}
		if j.keyed() {
			if seen[j.Key] {
				return fmt.Errorf("harness: GitHubRun has two jobs keyed %s", j.Key)
			}
			seen[j.Key] = true
		}
		for si, s := range j.Steps {
			if !s.Conclusion.IsGitHub() {
				return fmt.Errorf("harness: GitHubRun.Jobs[%d].Steps[%d] (%s) conclusion %q is not one of GitHub's eight", i, si, s.Name, s.Conclusion)
			}
		}
	}
	return nil
}
