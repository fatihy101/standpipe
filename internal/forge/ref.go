package forge

import "strings"

// ---------------------------------------------------------------------------
// SEAM 1 of 3 — the dispatch ref form. VERIFIED DEFAULT, escape hatch kept.
//
// ANSWERED BY MEASUREMENT on 2026-08-21. The "unverified" marker on
// the `ref` field is CLOSED. Do not reopen it as an investigation;
// if you need to change behaviour here, you need a NEW measurement, not the
// old question.
//
// WHAT WAS MEASURED. Four real workflow_dispatch calls against a private
// throwaway repo:
//
//	refs/heads/main   -> 200
//	refs/tags/v0.1    -> 200
//	v0.1  (short tag) -> 200
//
// So the endpoint accepts the FULL ref form for branches AND for tags, and it
// accepts short names too. RefFormFull is correct; no normalization step is
// needed; the code was not touched by that slice. Community discussion #75513
// ("the dispatch API accepts branches only, no tags") is wrong or stale, and
// an earlier design assumption rested on it.
//
// WHAT THE MEASUREMENT ALSO SETTLED — WHOSE RULE THIS IS. GitHub is LOOSER
// than we are. The gateway's refusal of a short name in the request body
// (IsFullRef) is therefore OUR POLICY, not an API constraint, and its
// reason is disambiguation: `v0.1` alone does not say branch or tag, a branch
// and a tag of the same name can coexist in one repo, and a wrong guess does
// not produce an error — it produces a dispatch that runs on the wrong ref and
// whose record looks correct. Read while the marker was still standing, that
// sentence looked like GitHub's restriction. It is not. GitHub being looser
// than us is not a reason for us to loosen.
//
// WHAT IS STILL NOT KNOWN. One repo, four calls, one moment. The sample says
// "the endpoint took these three forms today", not "it takes them under every
// condition". That is a named limit, not a marker.
//
// TO CORRECT: change DispatchRefForm below (one line). Nothing else in the
// package inspects the ref form on the way out — every dispatch path goes
// through DispatchRef. The seam stays because GitHub can change under us and a
// one-line switch is still the cheapest possible response; it is not here
// because the answer is in doubt.
// ---------------------------------------------------------------------------

// RefForm names the shape in which a ref is handed to the dispatch endpoint.
type RefForm int

const (
	// RefFormFull sends the canonical ref unchanged: refs/heads/main,
	// refs/tags/v1.
	RefFormFull RefForm = iota
	// RefFormShort strips the refs/heads/ or refs/tags/ prefix before the
	// call: main, v1.
	RefFormShort
)

// DispatchRefForm is the single switch that decides what goes on the wire.
//
// RefFormFull is the MEASURED-CORRECT setting, not a guess: the gateway
// defines the canonical request ref as a full path, forwards it unchanged
// ("ref": <request.ref>), and the 2026-08-21 probe answered 200
// for refs/heads/main and refs/tags/v0.1 alike. Flip to RefFormShort only if a
// NEW real call shows the endpoint has started rejecting full refs; that outcome
// would need a normalization step, and this switch is it.
const DispatchRefForm = RefFormFull

// DispatchRef converts a canonical gateway ref into the form sent to
// GitHub's dispatch endpoint. Every dispatch goes through here.
func DispatchRef(canonicalRef string) string {
	switch DispatchRefForm {
	case RefFormShort:
		return ShortRef(canonicalRef)
	default:
		return canonicalRef
	}
}

// Ref prefixes recognised by the gateway.
const (
	refsHeadsPrefix = "refs/heads/"
	refsTagsPrefix  = "refs/tags/"
)

// IsFullRef reports whether s is a full ref path the gateway accepts.
//
// A short name is not accepted in a request body. Measured 2026-08-21:
// GitHub itself DOES accept a short name. This rejection is the gateway's own
// disambiguation policy — see the seam comment above — so do not "fix" it to
// match the upstream behaviour.
func IsFullRef(s string) bool {
	return strings.HasPrefix(s, refsHeadsPrefix) || strings.HasPrefix(s, refsTagsPrefix)
}

// IsTagRef reports whether s names a tag.
//
// UNUSED TODAY, and honestly so: it exists because tags were the sharpest edge
// of the ref-form question — the sources that denied the endpoint took tags
// denied TAGS specifically — and a caller was expected to need the branch/tag
// distinction if full tag refs turned out to be rejected. The 2026-08-21
// measurement says they are not (refs/tags/v0.1 -> 200), so no caller ever
// appeared. Kept as part of the seam rather than deleted; it is removable
// if the seam is ever retired.
func IsTagRef(s string) bool { return strings.HasPrefix(s, refsTagsPrefix) }

// ShortRef strips a known ref prefix. Unknown shapes are returned unchanged —
// silently rewriting an unrecognised ref would be a quiet degradation.
func ShortRef(s string) string {
	if rest, ok := strings.CutPrefix(s, refsHeadsPrefix); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(s, refsTagsPrefix); ok {
		return rest
	}
	return s
}

// BranchRef turns a workflow run's head_branch into the gateway's canonical
// branch ref. GitHub reports head_branch as a short name ("main"); the
// correlation resolver and the coverage-gap report both need it as refs/heads/main to compare against target_ref.
//
// A run triggered on a tag also reports the tag in head_branch, with no
// ref_type to tell them apart, so this cannot be a general inverse of
// ShortRef — it is branch-only by construction, which is why it carries the
// branch name.
func BranchRef(headBranch string) string {
	if headBranch == "" {
		return ""
	}
	if IsFullRef(headBranch) {
		return headBranch
	}
	return refsHeadsPrefix + headBranch
}

// RefMatchesHeadBranch implements the ref normalization used by the
// fallback resolver: refs/heads/main ↔ main.
//
// It also accepts a bare tag match, because head_branch carries a tag name for
// tag-triggered runs. That is a widening, not a narrowing: a false positive
// here produces resolution "ambiguous", never a silent wrong link.
func RefMatchesHeadBranch(targetRef, headBranch string) bool {
	if targetRef == "" || headBranch == "" {
		return false
	}
	return ShortRef(targetRef) == ShortRef(headBranch)
}
