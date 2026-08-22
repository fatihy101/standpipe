package httpapi

import (
	"errors"
	"sync"
	"time"

	"github.com/fatihy101/standpipe/internal/forge"
)

// githubContact is what the admin health body's `github_reachable` is derived from.
//
// It exists because the field used to be derived from the rate limiter's
// observations, and that is a different fact. Measured 2026-08-22 against live
// api.github.com: a failed App JWT — 401, or the 404 GitHub answers for
// "Integration not found" — carries NO X-RateLimit-* headers at all. After
// seven such calls the gateway had spoken to GitHub seven times, GitHub had
// answered seven times, and `github_reachable` said false. The fake could not
// show this because it attaches rate headers to every 2xx and reality does not
// attach them to authenticated failures.
//
// The distinction is the whole value of the field to an operator: "GitHub is
// not answering" is the case the direct-call runbook covers, and "GitHub is answering and refusing
// our credential" is not — it is a key or an installation to fix, and reaching
// for the direct-call procedure wastes the outage on the wrong hypothesis.
type githubContact struct {
	mu sync.Mutex
	// at is when the recorded verdict was formed, zero if no GitHub call has
	// been classified yet. It is published so `false` before the first call
	// cannot be read as "GitHub is down" — absence of evidence has its own
	// shape on the wire.
	at        time.Time
	reachable bool
}

// contactEvidence is what one failed or successful GitHub call proves about
// reachability. The third value is the one that matters: most upstream errors
// prove nothing either way, and recording them would put the same lie back
// under a new derivation.
type contactEvidence int

const (
	// evidenceNone: this outcome says nothing about whether GitHub answered.
	evidenceNone contactEvidence = iota
	// evidenceAnswered: GitHub produced an HTTP response. A 401, a 404 and a
	// 422 all count — the question is reachability, not authorization.
	evidenceAnswered
	// evidenceNoAnswer: the call did not get a response out of GitHub.
	evidenceNoAnswer
)

// evidenceOf classifies one GitHub call outcome.
//
// The discriminator is forge.Error.UpstreamStatus, whose own documentation is
// exactly the predicate needed: "the GitHub HTTP status, or 0 if the call never
// produced a response". No change to internal/forge was required to read it.
func evidenceOf(err error) contactEvidence {
	if err == nil {
		return evidenceAnswered
	}
	var e *forge.Error
	if !errors.As(err, &e) {
		// Not an upstream error at all — a storage read, a context, our own
		// bug. Nothing was learned about GitHub.
		return evidenceNone
	}
	if e.UpstreamStatus != 0 {
		return evidenceAnswered
	}
	switch e.Kind {
	case forge.KindUnreachable:
		// DNS, dial, TLS, reset: the transport never reached GitHub.
		return evidenceNoAnswer
	case forge.KindUpstreamTimeout:
		// A timeout is ambiguous about the DISPATCH — it may have landed — but not
		// about reachability. No answer came back inside the budget, and that
		// is the fact this field reports.
		return evidenceNoAnswer
	default:
		// KindQueueWaitExceeded and KindQuotaPaused are OUR admission
		// decisions and never left the process; KindReservedInputKey is the
		// gateway's own 400. An installation or credential failure with no
		// status attached did not get far enough to say. None of these is
		// evidence, and treating them as unreachable would be the previous
		// defect wearing a different derivation.
		return evidenceNone
	}
}

func (g *githubContact) note(at time.Time, ev contactEvidence) {
	if ev == evidenceNone {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	// Last write wins on purpose: the field describes the CURRENT state, so a
	// recovery must be able to clear a failure and a new failure must be able
	// to clear a success.
	g.at = at
	g.reachable = ev == evidenceAnswered
}

func (g *githubContact) snapshot() (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.at, g.reachable
}

// NoteGitHubCall records what one ATTEMPTED GitHub call proved about
// reachability. It is the composition root's way of feeding github_reachable the calls
// that do not go through a request — bootstrap and the inventory refresh,
// which in a broken deployment are the only calls there are.
//
// Call it only where a call was actually attempted. A nil error is read as
// "GitHub answered", so passing the nil result of something that was served
// from a cache would manufacture a contact that never happened.
func (s *Server) NoteGitHubCall(err error) {
	s.github.note(s.now(), evidenceOf(err))
}
