package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
)

// The API error constants. These are the wire contract: a caller
// switches on them, so they are literals here and nowhere else.
const (
	ErrBadRequest          = "bad_request"
	ErrUnauthorized        = "unauthorized"
	ErrForbidden           = "forbidden"
	ErrIdempotencyConflict = "idempotency_conflict"
	ErrIdempotencyProgress = "idempotency_in_progress"
	ErrPayloadTooLarge     = "payload_too_large"
	ErrUnsupportedMedia    = "unsupported_media_type"
	ErrUnsupportedInputs   = "unsupported_inputs"
	ErrUpstreamRejected    = "upstream_rejected"
	ErrRateLimited         = "rate_limited"
	ErrInternal            = "internal_error"
	ErrUpstreamError       = "upstream_error"
	ErrBackendUnreachable  = "backend_unreachable"
	ErrStorageDegraded     = "storage_degraded"

	// ErrMethodNotAllowed has no row of its own in the error table — the
	// contract only says "405 Method Not Allowed is returned for non-POST
	// methods" without a body. The error body template requires an `error`
	// constant on every error body, so one is minted here.
	ErrMethodNotAllowed = "method_not_allowed"
)

// The `detail` constants that are fixed strings rather than free text.
const (
	DetailReservedInputKey     = "reserved_input_key"
	DetailDefaultBranchUnknown = "default_branch_unknown"
	DetailWorkflowNotFound     = "workflow_not_found"
	DetailInstallationNotFound = "installation_not_found"
	DetailCredentialExpired    = "credential_expired"

	// DetailAppNotFound was added after the original error table —
	// internal/forge minted
	// forge.KindAppNotFound on 2026-08-22 after measuring that GitHub answers
	// 404 "Integration not found" when github.app_id names no App at all, as
	// opposed to the bare 404 that means the lookup found no installation. It
	// maps to 503 backend_unreachable, the "identity problem" row.
	//
	// It is deliberately NOT DetailInstallationNotFound. Those two details are
	// the entire caller-visible carrier of a distinction forge established by
	// reading the response body, and the remedies do not overlap: no amount of
	// installing an App fixes an app_id that names none.
	DetailAppNotFound    = "app_not_found"
	DetailNotInAllowlist = "not_in_allowlist"
	DetailCallerPolicy   = "caller_policy"
	DetailUnknownPath    = "unknown_path"
)

// The fixed Retry-After values.
const (
	retryAfterInProgress = 5
	retryAfterInternal   = 5
	retryAfterDegraded   = 60
)

// apiError is one row of the error table, resolved.
//
// It carries the caller-facing shape AND the two facts the log needs — whether
// this outcome is a trigger_rejected rejection and under which reason — so the response
// and the event cannot describe the same refusal differently.
type apiError struct {
	Status int
	Code   string
	Detail string

	// RetryAfter is in seconds; 0 means no Retry-After header (the rule:
	// 429, 409 idempotency_in_progress, 500 and 503 carry one).
	RetryAfter int

	// UpstreamStatus is emitted only on 502 upstream_error.
	UpstreamStatus int
	// Repo is emitted only on 503 installation_not_found (the error table's row
	// `{"error": "backend_unreachable", "detail": "installation_not_found",
	// "repo": "acme/core"}`). upstreamError is the ONE constructor that sets
	// it; a repo on any other row would be a target name echoed where the
	// contract does not ask for one.
	Repo string
	// UnsupportedKeys is emitted only on 422 unsupported_inputs.
	UnsupportedKeys []string
}

// errorBody is the error body template. Optional members are omitted rather
// than sent as null: the contract marks them "optional" and a null would make a
// caller's `if body.retry_after_seconds` read true.
type errorBody struct {
	Error           string   `json:"error"`
	Detail          string   `json:"detail,omitempty"`
	RequestID       string   `json:"request_id"`
	RetryAfter      int      `json:"retry_after_seconds,omitempty"`
	UnsupportedKeys []string `json:"unsupported_keys,omitempty"`
	UpstreamStatus  int      `json:"upstream_status,omitempty"`
	Repo            string   `json:"repo,omitempty"`
}

func (e apiError) body(requestID string) errorBody {
	return errorBody{
		Error:           e.Code,
		Detail:          e.Detail,
		RequestID:       requestID,
		RetryAfter:      e.RetryAfter,
		UnsupportedKeys: e.UnsupportedKeys,
		UpstreamStatus:  e.UpstreamStatus,
		Repo:            e.Repo,
	}
}

// writeError renders an apiError. It is the only place an error response is
// produced, so the Retry-After rule cannot be applied in one place and
// forgotten in another.
func writeError(w http.ResponseWriter, requestID string, e apiError) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, e.Status, e.body(requestID))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		// Marshalling our own response types cannot fail; if it somehow does,
		// answering with a bare 500 beats answering with a truncated body.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
	_, _ = w.Write([]byte("\n"))
}

// --- the error table rows, as constructors ----------------------------------

func errBadRequest(detail string) apiError {
	return apiError{Status: http.StatusBadRequest, Code: ErrBadRequest, Detail: detail}
}

func errUnauthorized() apiError {
	// A 401 carries no detail. It stays that way: a detail here is an oracle
	// telling an attacker which of format, CRC, lookup or comparison failed.
	return apiError{Status: http.StatusUnauthorized, Code: ErrUnauthorized}
}

func errForbidden(detail string) apiError {
	// There is NO 404 anywhere in this surface: an unauthorized target
	// and a nonexistent one answer identically, so the response cannot be used
	// to enumerate repositories.
	return apiError{Status: http.StatusForbidden, Code: ErrForbidden, Detail: detail}
}

func errIdempotencyConflict(detail string) apiError {
	return apiError{Status: http.StatusConflict, Code: ErrIdempotencyConflict, Detail: detail}
}

func errIdempotencyInProgress() apiError {
	return apiError{Status: http.StatusConflict, Code: ErrIdempotencyProgress, RetryAfter: retryAfterInProgress}
}

func errPayloadTooLarge() apiError {
	return apiError{Status: http.StatusRequestEntityTooLarge, Code: ErrPayloadTooLarge}
}

func errUnsupportedMediaType() apiError {
	return apiError{Status: http.StatusUnsupportedMediaType, Code: ErrUnsupportedMedia}
}

func errUnsupportedInputs(keys []string) apiError {
	return apiError{Status: http.StatusUnprocessableEntity, Code: ErrUnsupportedInputs, UnsupportedKeys: keys}
}

func errRateLimited(retryAfterSeconds int, detail string) apiError {
	if retryAfterSeconds < 1 {
		retryAfterSeconds = 1
	}
	return apiError{Status: http.StatusTooManyRequests, Code: ErrRateLimited, RetryAfter: retryAfterSeconds, Detail: detail}
}

func errInternal(detail string) apiError {
	return apiError{Status: http.StatusInternalServerError, Code: ErrInternal, Detail: detail, RetryAfter: retryAfterInternal}
}

func errStorageDegraded() apiError {
	return apiError{Status: http.StatusServiceUnavailable, Code: ErrStorageDegraded, RetryAfter: retryAfterDegraded}
}

func errMethodNotAllowed() apiError {
	return apiError{Status: http.StatusMethodNotAllowed, Code: ErrMethodNotAllowed}
}

// storageError separates the proactive free-space threshold from a write that failed.
//
// This distinction is load-bearing and the two must not be collapsed:
//
//   - store.ErrStorageDegraded is the PROACTIVE check (CheckFreeSpace):
//     free space is below storage.min_free_bytes, nothing has been attempted,
//     and the honest answer is 503 storage_degraded + Retry-After 60 — come
//     back, an operator can fix this.
//   - a write that actually failed — including SQLITE_FULL, which
//     store.IsDiskFull reports — is 500 internal_error. Durable storage was
//     expected to work and did not, and the request's fate is unknown to us.
//
// Answering 503 for the second would tell the caller "retry later", when what
// really happened is that we may have half-written the log.
func storageError(err error) apiError {
	if errors.Is(err, store.ErrStorageDegraded) {
		return errStorageDegraded()
	}
	if store.IsDiskFull(err) {
		return errInternal("durable write failed: no space left")
	}
	return errInternal("durable write failed")
}

// upstreamError maps a forge.Kind onto the error table.
//
// It switches on the Kind and never on a number: with tier 1 armed a
// successful dispatch answers 200 rather than 204, so any predicate built on
// the numeric status is wrong for half of the deployments: a numeric HTTP
// status is never a success predicate.
//
// `repo` is the target of the request being answered, in owner/name form. It
// reaches the body on exactly one row — 503 installation_not_found — and it is
// a parameter rather than a field on the error because internal/forge does not
// carry the repo on *forge.Error. Every call site is past step 11, so the
// value has already been matched against allowed_targets: this echoes a name
// the caller sent AND the server-side allowlist carries, so it cannot be used
// to enumerate anything the caller could not already name.
func upstreamError(err error, repo string) apiError {
	var e *forge.Error
	if !errors.As(err, &e) {
		return apiError{Status: http.StatusBadGateway, Code: ErrUpstreamError, Detail: "upstream call failed"}
	}
	switch e.Kind {
	case forge.KindWorkflowNotFound:
		// An upstream 404 is a 502 for the caller, never a 404 — this
		// surface has no 404.
		return apiError{Status: http.StatusBadGateway, Code: ErrUpstreamError, Detail: DetailWorkflowNotFound}

	case forge.KindUpstreamRejected:
		// 422 with GitHub's own message.
		return apiError{Status: http.StatusUnprocessableEntity, Code: ErrUpstreamRejected, Detail: e.Message}

	case forge.KindSecondaryRateLimit, forge.KindPrimaryRateLimit:
		// GitHub is throttling US. It is answered 429 because that is the
		// status the error table gives the `rate_limited` constant and the one
		// internal/forge documents, but the detail names the upstream cause:
		// Caller rate limiting is not the GitHub limiter: never CONFLATE this with the inbound caller
		// limiter, and the distinguishing surface is the detail plus the
		// metrics — a caller limit raises gateway_trigger_rejected_total and
		// writes a trigger_rejected event, this raises
		// gateway_upstream_errors_total and writes a trigger_outcome.
		return apiError{
			Status:     http.StatusTooManyRequests,
			Code:       ErrRateLimited,
			Detail:     "upstream_" + string(e.Kind),
			RetryAfter: seconds(e.RetryAfter, 60),
		}

	case forge.KindInstallationNotFound:
		// The error table names `repo` on this row and only this row. Without it the
		// operator reading the caller's failure has to correlate a request id
		// against the log to learn WHICH repo the App is missing from, which
		// is the whole content of the "App not installed on this repo" state.
		return apiError{Status: http.StatusServiceUnavailable, Code: ErrBackendUnreachable,
			Detail: DetailInstallationNotFound, RetryAfter: 60, Repo: repo}

	case forge.KindAppNotFound:
		// 503 backend_unreachable, the same row as credential_expired: OUR
		// identity is unusable. Not 502 — a 502 says GitHub misbehaved, and
		// GitHub answered correctly and quickly; the fault is github.app_id.
		//
		// No `repo`: the error table gives that field to installation_not_found, and this
		// failure is not repo-scoped at all. Every repo fails identically
		// because the App does not exist, and naming one would point the
		// operator at the target instead of at the config.
		//
		// Retry-After is the credential row's 30s and means what it means
		// there: come back, an operator can fix this. It is not a claim that
		// waiting helps.
		return apiError{Status: http.StatusServiceUnavailable, Code: ErrBackendUnreachable,
			Detail: DetailAppNotFound, RetryAfter: 30}

	case forge.KindCredentialExpired:
		return apiError{Status: http.StatusServiceUnavailable, Code: ErrBackendUnreachable,
			Detail: DetailCredentialExpired, RetryAfter: 30}

	case forge.KindUnreachable:
		return apiError{Status: http.StatusServiceUnavailable, Code: ErrBackendUnreachable,
			Detail: "github api unreachable", RetryAfter: 15}

	case forge.KindQueueWaitExceeded:
		// The in-flight queue is full. 503 + Retry-After, per the config
		// reference's note on github.max_queue_wait.
		return apiError{Status: http.StatusServiceUnavailable, Code: ErrBackendUnreachable,
			Detail: string(forge.KindQueueWaitExceeded), RetryAfter: 5}

	case forge.KindUpstreamServerError, forge.KindUpstreamTimeout:
		// 502 for a GitHub 5xx or a timeout.
		return apiError{Status: http.StatusBadGateway, Code: ErrUpstreamError,
			Detail: string(e.Kind), UpstreamStatus: e.UpstreamStatus}

	case forge.KindForbidden:
		// A 403 that is not a rate limit: the installation lacks permission.
		return apiError{Status: http.StatusBadGateway, Code: ErrUpstreamError,
			Detail: "upstream_forbidden", UpstreamStatus: e.UpstreamStatus}

	default:
		return apiError{Status: http.StatusBadGateway, Code: ErrUpstreamError,
			Detail: string(e.Kind), UpstreamStatus: e.UpstreamStatus}
	}
}

func seconds(d time.Duration, fallback int) int {
	if d <= 0 {
		return fallback
	}
	s := int(d.Round(time.Second) / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}
