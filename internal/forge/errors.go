package forge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/go-github/v90/github"
)

// Kind is the closed set of upstream failure classes this package produces.
// The HTTP layer maps a Kind onto the trigger endpoint's status codes; the
// mapping is differentiated on purpose — a bare "upstream error" would hide
// the difference between a permanently wrong target and a transient outage.
type Kind string

const (
	// KindWorkflowNotFound is an upstream 404: the workflow does not exist or
	// the installation cannot see it. Caller-visible as 502 workflow_not_found.
	// Cacheable — retrying will not change the answer.
	KindWorkflowNotFound Kind = "workflow_not_found"

	// KindUpstreamRejected is an upstream 422: invalid ref, unexpected input.
	// Caller-visible as 422 upstream_rejected with GitHub's message. Cacheable.
	KindUpstreamRejected Kind = "upstream_rejected"

	// KindSecondaryRateLimit is GitHub's secondary rate limit. It
	// arrives as 403, not 429, and carries Retry-After. Caller-visible as 429.
	KindSecondaryRateLimit Kind = "secondary_rate_limit"

	// KindPrimaryRateLimit is the primary hourly quota being exhausted.
	// Caller-visible as 429.
	KindPrimaryRateLimit Kind = "primary_rate_limit"

	// KindForbidden is a 403 that is neither rate limit: the installation
	// lacks the permission. Caller-visible as 502 upstream_error.
	KindForbidden Kind = "forbidden"

	// KindUpstreamServerError is an upstream 5xx. Leaves the dispatch unknown.
	KindUpstreamServerError Kind = "upstream_server_error"

	// KindUpstreamTimeout is the upstream timeout. Leaves the dispatch unknown.
	// It is NEVER retried automatically — the timeout is ambiguous, the
	// dispatch may already have landed, and the correlation resolver settles it.
	KindUpstreamTimeout Kind = "upstream_timeout"

	// KindUnreachable is a transport failure: DNS, dial, TLS, reset.
	// Caller-visible as 503 backend_unreachable.
	KindUnreachable Kind = "backend_unreachable"

	// KindInstallationNotFound means the App is not installed on the target
	// repo. Caller-visible as 503 installation_not_found.
	KindInstallationNotFound Kind = "installation_not_found"
	// KindCredentialExpired means the installation token expired and could not
	// be renewed. Caller-visible as 503 credential_expired.
	KindCredentialExpired Kind = "credential_expired"

	// KindQueueWaitExceeded means the request waited longer than
	// github.max_queue_wait for an in-flight slot. 503 + Retry-After.
	KindQueueWaitExceeded Kind = "queue_wait_exceeded"

	// KindQuotaPaused means background inventory work is paused for this
	// installation because rate_remaining fell below the threshold.
	// It never reaches a caller — live dispatch is not paused.
	KindQuotaPaused Kind = "quota_paused"

	// KindReservedInputKey means the caller sent standby_trigger_id itself
	// (injection rule 3). This is the gateway's own decision, not an upstream
	// rejection: 400 bad_request, detail reserved_input_key.
	KindReservedInputKey Kind = "reserved_input_key"

	// KindUnknown is the fallback. Treated as leaving the dispatch unknown.
	KindUnknown Kind = "unknown"
)

// Error is the typed error every exported function in this package returns for
// an upstream or admission failure.
type Error struct {
	// Kind drives the caller-facing mapping.
	Kind Kind
	// UpstreamStatus is the GitHub HTTP status, or 0 if the call never
	// produced a response.
	UpstreamStatus int
	// Message is GitHub's own message where there is one. It is surfaced to
	// the caller only for KindUpstreamRejected.
	Message string
	// RetryAfter is set for KindSecondaryRateLimit and KindPrimaryRateLimit.
	RetryAfter time.Duration
	// Err is the wrapped cause.
	Err error
}

func (e *Error) Error() string {
	switch {
	case e.Message != "" && e.UpstreamStatus != 0:
		return fmt.Sprintf("github: %s (status %d): %s", e.Kind, e.UpstreamStatus, e.Message)
	case e.UpstreamStatus != 0:
		return fmt.Sprintf("github: %s (status %d)", e.Kind, e.UpstreamStatus)
	case e.Err != nil:
		return fmt.Sprintf("github: %s: %v", e.Kind, e.Err)
	default:
		return "github: " + string(e.Kind)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// LeavesUnknown reports whether a dispatch that failed this way may still have
// landed on GitHub. Only 5xx and timeouts do (the upstream-4xx cache rule).
// A 404 or 422 is a definitive refusal: the trigger did not happen.
func (e *Error) LeavesUnknown() bool {
	switch e.Kind {
	case KindUpstreamServerError, KindUpstreamTimeout, KindUnreachable, KindUnknown:
		return true
	default:
		return false
	}
}

// Cacheable reports whether the outcome may be recorded against the
// idempotency key so the caller does not retry (the upstream-4xx cache rule).
func (e *Error) Cacheable() bool {
	switch e.Kind {
	case KindWorkflowNotFound, KindUpstreamRejected, KindReservedInputKey:
		return true
	default:
		return false
	}
}

// Sentinels for errors.Is. Kind is the discriminator; these exist so callers
// that only care about one case do not have to switch.
var (
	ErrQueueWaitExceeded  = &Error{Kind: KindQueueWaitExceeded}
	ErrQuotaPaused        = &Error{Kind: KindQuotaPaused}
	ErrCredentialExpired  = &Error{Kind: KindCredentialExpired}
	ErrInstallationAbsent = &Error{Kind: KindInstallationNotFound}
	ErrReservedInputKey   = &Error{Kind: KindReservedInputKey}
)

// Is lets errors.Is match on Kind alone, so errors.Is(err, ErrQueueWaitExceeded)
// works against a fully populated *Error.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Kind == e.Kind
}

// KindOf extracts the Kind from any error this package produced.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if err == nil {
		return ""
	}
	return KindUnknown
}

// classify turns a go-github error plus its response into an *Error.
//
// The order matters: go-github reports the secondary rate limit as its own
// type even though the wire status is 403, so it must be tested before the
// generic 403 branch.
func classify(err error, resp *github.Response) *Error {
	if err == nil {
		return nil
	}

	// Already ours (e.g. from the limiter or the identity layer).
	var already *Error
	if errors.As(err, &already) {
		return already
	}

	// Secondary rate limit: 403 with Retry-After, not 429.
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		out := &Error{
			Kind:           KindSecondaryRateLimit,
			UpstreamStatus: http.StatusForbidden,
			Message:        abuse.Message,
			Err:            err,
		}
		if abuse.RetryAfter != nil {
			out.RetryAfter = *abuse.RetryAfter
		}
		if out.RetryAfter <= 0 {
			out.RetryAfter = 60 * time.Second
		}
		return out
	}

	// Primary quota exhausted.
	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		retry := time.Until(rate.Rate.Reset.Time)
		if retry < 0 {
			retry = time.Second
		}
		return &Error{
			Kind:           KindPrimaryRateLimit,
			UpstreamStatus: statusOf(resp),
			Message:        rate.Message,
			RetryAfter:     retry,
			Err:            err,
		}
	}

	// Timeout — deliberately before the transport branch, and deliberately
	// never retried: the dispatch may already have landed.
	if isTimeout(err) {
		return &Error{Kind: KindUpstreamTimeout, Err: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: KindUnreachable, Err: err}
	}

	var apiErr *github.ErrorResponse
	if errors.As(err, &apiErr) {
		status := http.StatusOK
		if apiErr.Response != nil {
			status = apiErr.Response.StatusCode
		}
		return fromStatus(status, apiErr.Message, err)
	}

	if s := statusOf(resp); s != 0 {
		return fromStatus(s, "", err)
	}

	// No response at all: transport-level failure.
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, os.ErrDeadlineExceeded) {
		return &Error{Kind: KindUnreachable, Err: err}
	}
	return &Error{Kind: KindUnknown, Err: err}
}

func fromStatus(status int, message string, cause error) *Error {
	switch {
	case status == http.StatusNotFound:
		// 404 → workflow_not_found, cached, caller does not retry.
		return &Error{Kind: KindWorkflowNotFound, UpstreamStatus: status, Message: message, Err: cause}
	case status == http.StatusUnprocessableEntity:
		// 422 → upstream_rejected, GitHub's message is surfaced.
		return &Error{Kind: KindUpstreamRejected, UpstreamStatus: status, Message: message, Err: cause}
	case status == http.StatusForbidden:
		return &Error{Kind: KindForbidden, UpstreamStatus: status, Message: message, Err: cause}
	case status == http.StatusUnauthorized:
		// A rejected installation token: the credential is no longer good.
		return &Error{Kind: KindCredentialExpired, UpstreamStatus: status, Message: message, Err: cause}
	case status >= 500:
		// Only 5xx and timeout leave the dispatch unknown.
		return &Error{Kind: KindUpstreamServerError, UpstreamStatus: status, Message: message, Err: cause}
	default:
		return &Error{Kind: KindUnknown, UpstreamStatus: status, Message: message, Err: cause}
	}
}

func statusOf(resp *github.Response) int {
	if resp == nil || resp.Response == nil {
		return 0
	}
	return resp.StatusCode
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
