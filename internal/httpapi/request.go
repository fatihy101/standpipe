package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxReasonRunes is the cap on `reason`.
const maxReasonRunes = 1024

// maxIdempotencyKeyBytes is the cap on the Idempotency-Key header.
const maxIdempotencyKeyBytes = 128

// triggerBody is the canonical trigger request schema.
//
// Inputs is map[string]any rather than map[string]string so a caller sending
// a number or a boolean gets an honest 400 naming the key, instead of the JSON
// decoder rejecting the whole body with a type error that names nothing.
type triggerBody struct {
	Ref     string          `json:"ref"`
	Inputs  map[string]any  `json:"inputs"`
	Reason  string          `json:"reason"`
	Require json.RawMessage `json:"require"`
}

// requestIDRE is the X-Request-Id charset.
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestID honours a client-supplied X-Request-Id, or mints one.
//
// The contract caps it at 64 characters and restricts it to [A-Za-z0-9._-], and the
// stated reason is log/header injection. That is the whole defence and it has
// to be here: the value is echoed into a response HEADER and into every
// structured log line for this request, so a value containing CR, LF or a
// quote would let a caller forge a log record or split a header. Anything that
// does not match is REPLACED — not sanitised, not truncated — because a
// partial value that looks like the caller's own id is worse than an obviously
// new one.
func requestID(r *http.Request, mint func() string) string {
	if v := r.Header.Get("X-Request-Id"); requestIDRE.MatchString(v) {
		return v
	}
	return mint()
}

// contentTypeOK is step 3 of the request-processing order.
func contentTypeOK(h string) bool {
	if h == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(h)
	if err != nil {
		return false
	}
	return strings.EqualFold(mt, "application/json")
}

// clientIP is step 5: the rightmost address in X-Forwarded-For
// that is NOT one of our own trusted proxies.
//
// Walking from the right is the only correct direction. The left end of the
// header is whatever the client wrote, so trusting it lets any caller pick its
// own rate-limit bucket; the right end is what our own proxy appended. If no
// proxy is configured, the header is ignored entirely.
//
// # One address, one spelling
//
// Every parseable return path here ends in Unmap().String(), so a given
// address has exactly one spelling in source_ip, in the pre-auth limiter key
// and in the watch-list match. That is a correctness property, not tidiness:
// the XFF and X-Real-Ip paths used to return the un-Unmapped form, so the same
// host arriving direct and arriving proxied as ::ffff:192.0.2.7 produced two
// limiter buckets and two source_ip values, and config's canonical
// watch_ips entry matched only one of them. An unparseable hop is still
// returned verbatim — it is not an address, and inventing one would be worse.
func clientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := hostOf(r.RemoteAddr)
	if len(trusted) == 0 {
		return remote
	}
	if !isTrusted(remote, trusted) {
		// The immediate peer is not a proxy we trust, so it cannot be
		// permitted to speak for anyone else.
		return remote
	}

	chain := forwardedChain(r)
	for i := len(chain) - 1; i >= 0; i-- {
		if !isTrusted(chain[i], trusted) {
			return chain[i]
		}
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-Ip")); v != "" {
		if addr, err := netip.ParseAddr(v); err == nil {
			return addr.Unmap().String()
		}
	}
	return remote
}

func forwardedChain(r *http.Request) []string {
	var out []string
	for _, header := range r.Header.Values("X-Forwarded-For") {
		for _, part := range strings.Split(header, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if addr, err := netip.ParseAddr(part); err == nil {
				out = append(out, addr.Unmap().String())
			} else {
				// An unparseable hop still counts as a hop: skipping it would
				// let a caller hide the real chain by writing garbage.
				out = append(out, part)
			}
		}
	}
	return out
}

func isTrusted(ip string, trusted []netip.Prefix) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func hostOf(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return host
}

// parsePrefixes converts gateway.trusted_proxies. Validation has already
// rejected anything unparseable, so an error here can only mean the config was
// bypassed.
func parsePrefixes(cidrs []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		if p, err := netip.ParsePrefix(c); err == nil {
			out = append(out, p.Masked())
		}
	}
	return out
}

// decodeSyntax is step 4: does the body parse at all.
//
// It is deliberately separate from validateBody. Step 4 runs BEFORE
// authentication, so its 400 must never reach durable storage; step 11
// runs after, and its 400 is a `trigger_rejected` event. Merging them would
// either write unauthenticated requests to the database or lose the
// authenticated ones from the log.
func decodeSyntax(raw []byte) error {
	var probe map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&probe); err != nil {
		return fmt.Errorf("invalid JSON body: %v", err)
	}
	if dec.More() {
		return errors.New("body must contain exactly one JSON object")
	}
	return nil
}

// validateBody is step 11 plus the body's field rules.
func validateBody(raw []byte) (triggerBody, map[string]string, error) {
	var body triggerBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return body, nil, fmt.Errorf("invalid trigger body: %v", err)
	}

	if body.Ref == "" {
		return body, nil, errors.New("field \"ref\" is required")
	}
	if err := validateFullRef(body.Ref); err != nil {
		return body, nil, err
	}

	if err := validateReason(body.Reason); err != nil {
		return body, nil, err
	}
	if !utf8.ValidString(body.Ref) {
		return body, nil, errors.New("body must be valid UTF-8")
	}

	inputs, err := stringInputs(body.Inputs)
	if err != nil {
		return body, nil, err
	}

	if err := validateRequire(body.Require); err != nil {
		return body, nil, err
	}
	return body, inputs, nil
}

// validateReason is the rule for the `reason` field, on its own.
//
// It is factored out of validateBody because trigger_rejected's reason_text admission
// (reqCtx.admitReason) has to apply the SAME rule to a body that FAILED
// validation — the value may be the very thing that was wrong. A second copy
// of "1024 runes and valid UTF-8" would be free to drift from this one, and
// the drift would show up as a column holding a value the schema forbids.
func validateReason(reason string) error {
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return fmt.Errorf("field \"reason\" exceeds %d characters", maxReasonRunes)
	}
	if !utf8.ValidString(reason) {
		return errors.New("body must be valid UTF-8")
	}
	return nil
}

// validateFullRef: a short ref name is not accepted.
//
// Rejecting "main" here rather than passing it through is what stops the
// caller's policy (which matches full refs) from silently never matching
// and producing a 403 that looks like a permissions problem.
func validateFullRef(ref string) error {
	switch {
	case strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/"):
	case strings.HasPrefix(ref, "refs/tags/") && len(ref) > len("refs/tags/"):
	default:
		return fmt.Errorf("field \"ref\" must be a full ref such as refs/heads/main or refs/tags/v1, got %q", ref)
	}
	if strings.Contains(ref, "..") || strings.ContainsAny(ref, " \t\r\n\\^~:?*[") {
		return fmt.Errorf("field \"ref\" contains characters a git ref may not carry: %q", ref)
	}
	return nil
}

// stringInputs flattens the body's inputs to strings.
//
// The schema says: "Values are strings — GitHub serialises workflow_dispatch
// inputs to strings. Typed inputs (bool, number) are sent as strings too."
// A JSON number or boolean is therefore accepted and rendered; an object, an
// array or null is refused, because there is no defined rendering for those
// and picking one would send GitHub something the caller did not write.
func stringInputs(in map[string]any) (map[string]string, error) {
	if in == nil {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == "" {
			return nil, errors.New("field \"inputs\" contains an empty key")
		}
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			if t {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		case float64:
			if math.Trunc(t) == t && math.Abs(t) < 1e15 {
				out[k] = fmt.Sprintf("%d", int64(t))
			} else {
				out[k] = fmt.Sprintf("%v", t)
			}
		case json.Number:
			out[k] = t.String()
		default:
			return nil, fmt.Errorf("field \"inputs\" value for key %q must be a string, number or boolean", k)
		}
	}
	return out, nil
}

// validateRequire: `require` is accepted in Phase 0, recorded as
// require_raw and NOT forwarded, and the only legal value is
// {"backend":"any"}. Anything else is 400.
func validateRequire(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("field \"require\" must be an object: %v", err)
	}
	if len(r) != 1 {
		return errors.New("field \"require\" accepts only {\"backend\":\"any\"} in Phase 0")
	}
	backend, ok := r["backend"].(string)
	if !ok || backend != "any" {
		return errors.New("field \"require\" accepts only {\"backend\":\"any\"} in Phase 0")
	}
	return nil
}

// validateIdempotencyKey is the Idempotency-Key header rule.
func validateIdempotencyKey(key string) error {
	switch {
	case key == "":
		return errors.New("header \"Idempotency-Key\" is required")
	case len(key) > maxIdempotencyKeyBytes:
		return fmt.Errorf("header \"Idempotency-Key\" exceeds %d bytes", maxIdempotencyKeyBytes)
	case !utf8.ValidString(key):
		return errors.New("header \"Idempotency-Key\" must be valid UTF-8")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return errors.New("header \"Idempotency-Key\" must not contain control characters")
		}
	}
	return nil
}
