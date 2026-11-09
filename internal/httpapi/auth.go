package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/fatihy101/standpipe/internal/config"
)

// tokenIDRE is the charset a token_id may use ([A-Za-z0-9_-]{1,64}; see
// docs/reference/tokens.md). It
// matches internal/config's rule, so a token this package mints is one the
// config file can name.
var tokenIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// tokenSecretRE is the token's 32-byte base62 body: EXACTLY 43 characters.
//
// The width is exact, not a floor. `{43,}` accepts a token-shaped string that
// is not a token, and — the reason the format spells it out — it would keep matching
// silently if the secret width ever changed, so the drift would surface
// nowhere. An exact width turns that drift into an error at mint time.
var tokenSecretRE = regexp.MustCompile(`^[A-Za-z0-9]{43}$`)

// tokenChecksumRE is the token's crc32 field: lower-case base36, unpadded, 1-7
// characters (uint32 max is "1z141z3", seven digits).
var tokenChecksumRE = regexp.MustCompile(`^[0-9a-z]{1,7}$`)

// tokenPrefix is the token's fixed prefix. `spd` is Standpipe; it
// marks the token TYPE, not the product, and a second token type would get its
// own prefix and its own published regex rather than being buried inside
// <token_id>.
const tokenPrefix = "spd"

// tokenScanRE is, character for character, the regex published for secret
// scanning:
//
//	spd_[A-Za-z0-9_-]{1,64}_[A-Za-z0-9]{43}_[0-9a-z]{1,7}
//
// It is unanchored on purpose — a scanner extracts tokens from the middle of a
// log line — so every caller here compares FindString against the whole input
// rather than using MatchString. Running it at mint time is normative:
// `token create` refuses to emit a token this regex does not match, which is
// the mechanism that catches a divergence between generator and scanner. A
// private, more permissive copy of the pattern would defeat that, which is why
// this value is asserted against docs/reference/tokens.md by an executable test.
var tokenScanRE = regexp.MustCompile(`spd_[A-Za-z0-9_-]{1,64}_[A-Za-z0-9]{43}_[0-9a-z]{1,7}`)

// matchesScanRegex reports whether the WHOLE of s is what the published
// regex matches. A partial hit is what a scanner reports as a finding it then
// cannot revoke.
func matchesScanRegex(s string) bool { return tokenScanRE.FindString(s) == s }

// splitToken parses spd_<token_id>_<base62(32B)>_<crc32> RIGHT to left.
//
// Left-to-right is ambiguous the moment a token_id contains an underscore, and
// every example token_id in the documentation does. The last two segments are fixed by
// construction — the checksum and the 43-character secret — so everything
// between the "spd" prefix and them is the token id, whatever separators it
// holds.
func splitToken(token string) (tokenID, secret, checksum string, ok bool) {
	parts := strings.Split(token, "_")
	if len(parts) < 4 || parts[0] != tokenPrefix {
		return "", "", "", false
	}
	checksum = parts[len(parts)-1]
	secret = parts[len(parts)-2]
	tokenID = strings.Join(parts[1:len(parts)-2], "_")
	if !tokenSecretRE.MatchString(secret) || !tokenChecksumRE.MatchString(checksum) {
		return "", "", "", false
	}
	if !tokenIDRE.MatchString(tokenID) {
		return "", "", "", false
	}
	return tokenID, secret, checksum, true
}

// Caller is an authenticated caller.
type Caller struct {
	// Name is the idempotency namespace. It is keyed on the caller
	// NAME and not the token id, so rotating a token keeps the namespace.
	Name    string
	TokenID string
	Role    string
	Allow   []config.Allow
}

// IsAdmin reports whether this caller may read /inventory.
func (c Caller) IsAdmin() bool { return c.Role == config.RoleAdmin }

// authenticate performs the four authentication steps against a snapshot of the config.
//
// It returns only ok/not-ok. Every failure mode — malformed header, bad
// format, bad CRC, unknown token id, wrong secret — produces the same 401 with
// no detail, because the differences are exactly what an attacker would use to
// narrow a search.
func authenticate(cfg *config.Config, authorization string) (Caller, bool) {
	const prefix = "Bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return Caller{}, false
	}
	token := authorization[len(prefix):]

	// Step 1: format.
	tokenID, secret, checksum, ok := splitToken(token)
	if !ok {
		return Caller{}, false
	}

	// Step 2: CRC32 over everything before the checksum segment. This catches
	// a copy-paste truncation before it looks like a wrong secret.
	want := crc32.ChecksumIEEE([]byte(tokenPrefix + "_" + tokenID + "_" + secret))
	got, err := parseBase36(checksum)
	if err != nil || got != want {
		return Caller{}, false
	}

	// Step 3: token_id -> caller. The map is built from the snapshot, so a
	// SIGHUP mid-request cannot change the answer under this request's feet.
	name, entry, found := callerByTokenID(cfg, tokenID)
	if !found {
		return Caller{}, false
	}

	// Step 4: constant-time comparison of the SHA-256.
	sum := sha256.Sum256([]byte(token))
	wantHash, err := hex.DecodeString(strings.TrimPrefix(entry.TokenHash, "sha256:"))
	if err != nil || len(wantHash) != len(sum) {
		return Caller{}, false
	}
	if subtle.ConstantTimeCompare(sum[:], wantHash) != 1 {
		return Caller{}, false
	}

	return Caller{Name: name, TokenID: entry.TokenID, Role: entry.Role, Allow: entry.Allow}, true
}

// parseBase36 decodes the token's crc32 field.
//
// LOWER-CASE BASE36 ONLY. Authentication step 2 forbids trying a second encoding by
// name: "1ar9ad" is a valid string in both base36 and hex and decodes to two
// different numbers, so a decoder that falls back from one to the other turns
// the integrity check into "some base agreed", which is not a check. The
// alphabet is already pinned by tokenChecksumRE; this function refuses
// anything outside it rather than folding case, because folding case would
// accept a checksum the published regex rejects.
func parseBase36(s string) (uint32, error) {
	if !tokenChecksumRE.MatchString(s) {
		return 0, fmt.Errorf("checksum %q is not lower-case base36", s)
	}
	v, err := strconv.ParseUint(s, 36, 32)
	return uint32(v), err
}

func callerByTokenID(cfg *config.Config, tokenID string) (string, config.Caller, bool) {
	for name, c := range cfg.Callers {
		if c.TokenID == tokenID {
			return name, c, true
		}
	}
	return "", config.Caller{}, false
}

// authorizeCaller is the caller policy: the (repo, workflow, ref) triple must match at
// least one entry of the caller's allow list.
func authorizeCaller(c Caller, repo, workflow, ref string) bool {
	for _, a := range c.Allow {
		if a.Repo == repo && a.Workflow == workflow && matchRef(a.Ref, ref) {
			return true
		}
	}
	return false
}

// targetResult is the outcome of the server-side allowlist check.
type targetResult int

const (
	targetAllowed targetResult = iota
	targetDenied
	// targetNeedsDefaultBranch means the entry declares no `refs`, so the
	// default applies: the default branch only. The caller must supply it.
	targetNeedsDefaultBranch
)

// authorizeTarget is the server-side allowlist. It is the SECOND layer: a caller whose own policy
// passes still has to be inside the server-side allowlist.
func authorizeTarget(targets []config.AllowedTarget, repo, workflow, ref string) targetResult {
	for _, t := range targets {
		if t.Repo != repo {
			continue
		}
		if !containsString(t.Workflows, workflow) {
			continue
		}
		if len(t.Refs) == 0 {
			// "If refs is not given: the default branch only". Fail
			// closed — resolving it needs the default branch, and if that is
			// unknown the answer is 403 default_branch_unknown, never "allow".
			return targetNeedsDefaultBranch
		}
		for _, pattern := range t.Refs {
			if matchRef(pattern, ref) {
				return targetAllowed
			}
		}
	}
	return targetDenied
}

// matchRef implements the allowlist's glob semantics, and it is the SAME function
// the caller policy uses — the design requires that explicitly, because two matchers would drift
// and the drift would show up as an authorization difference.
//
//   - "*" alone allows every ref.
//   - otherwise "*" matches within ONE path segment; it does not cross "/".
//   - "**" is not supported (config validation rejects it).
func matchRef(pattern, ref string) bool {
	if pattern == "*" {
		return true
	}
	if pattern == ref {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	ok, err := path.Match(pattern, ref)
	return err == nil && ok
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
