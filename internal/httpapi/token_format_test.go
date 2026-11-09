package httpapi

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The caller-token format and its secret-scanning regex, executed.
//
// The scanning regex is published in docs/reference/tokens.md, which is
// generated from TokenScanPattern(), i.e. from tokenScanRE itself. There is
// therefore exactly one copy of the pattern, and MintToken runs it against its
// own output before emitting a token: if the generator and the regex ever
// diverge, minting refuses rather than printing a token a scanner cannot see.
//
// The tests below are that guarantee measured: real minted tokens must FULLY
// match, near misses must not, and a minted token must authenticate.
// ---------------------------------------------------------------------------

// TestTokenScanPatternIsWhatTheMinterEnforces pins that the pattern exposed
// for publication is the one matchesScanRegex uses.
func TestTokenScanPatternIsWhatTheMinterEnforces(t *testing.T) {
	if got, want := TokenScanPattern(), tokenScanRE.String(); got != want {
		t.Fatalf("TokenScanPattern() = %q, but MintToken enforces %q", got, want)
	}
	if _, err := regexp.Compile(TokenScanPattern()); err != nil {
		t.Fatalf("the published scanning regex does not compile: %v", err)
	}
	if !strings.HasPrefix(TokenScanPattern(), TokenPrefix()+"_") {
		t.Errorf("the scanning regex %q does not start with the token prefix %q", TokenScanPattern(), TokenPrefix())
	}
}

// TestMintedTokensMatchThePublishedRegex mints a few hundred tokens and
// requires every one to match the published regex in full.
//
// It asserts a FULL match, not a match: a partial hit is what a scanner
// reports as a finding it then cannot revoke.
func TestMintedTokensMatchThePublishedRegex(t *testing.T) {
	re := regexp.MustCompile(TokenScanPattern())

	// The token ids the documentation uses, plus the shapes the alphabet
	// allows: underscores, hyphens, digits, and the 1- and 64-character width
	// bounds.
	ids := []string{
		"tok_a1", "tok_b2", "tok_admin",
		"a", "Z", "9",
		"release-bot", "release_bot-2026", "A-b_C-9",
		strings.Repeat("x", 64),
		strings.Repeat("a_", 32)[:63] + "z",
	}
	for i := 0; i < 200; i++ {
		ids = append(ids, randomTokenID(t))
	}

	var missed, minted int
	var examples []string
	for _, id := range ids {
		token, hash, err := MintToken(id)
		if err != nil {
			t.Errorf("MintToken(%q) refused to mint: %v", id, err)
			continue
		}
		minted++
		if !strings.HasPrefix(hash, "sha256:") {
			t.Errorf("MintToken(%q) hash %q is not sha256-prefixed", id, hash)
		}
		if re.FindString(token) != token {
			missed++
			if len(examples) < 3 {
				examples = append(examples, fmt.Sprintf("token_id=%q token=%s matched=%q", id, token, re.FindString(token)))
			}
		}
	}
	if missed > 0 {
		t.Errorf("the published scanning regex misses %d of %d REAL tokens this build mints.\n"+
			"  published: %s\n"+
			"  examples:\n    %s\n"+
			"A leaked token the published scanner cannot see defeats the point of publishing it.",
			missed, minted, re.String(), strings.Join(examples, "\n    "))
	}
}

// TestPublishedRegexRejectsNearMisses is the false-positive half: a 42- or
// 44-character secret and an upper-case checksum are NOT tokens, and a regex
// that accepts them has stopped fixing the field widths.
func TestPublishedRegexRejectsNearMisses(t *testing.T) {
	re := regexp.MustCompile(TokenScanPattern())

	token, _, err := MintToken("tok_a1")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	prefix, id, secret, checksum := splitForTest(t, token)

	cases := []struct {
		name string
		s    string
	}{
		{"secret one short (42)", prefix + "_" + id + "_" + secret[:42] + "_" + checksum},
		{"secret one long (44)", prefix + "_" + id + "_" + secret + "A" + "_" + checksum},
		{"upper-case checksum", prefix + "_" + id + "_" + secret + "_" + strings.ToUpper(checksum) + "Z"},
		{"empty token id", prefix + "__" + secret + "_" + checksum},
	}
	for _, tc := range cases {
		if got := re.FindString(tc.s); got == tc.s {
			t.Errorf("%s: the published regex matched the WHOLE of a non-token: %s", tc.name, tc.s)
		}
	}
}

// TestMintRefusesAnIDTheParserCannotRoundTrip is the other half of the
// mint-time self-check: the minter must REFUSE, not warn.
//
// Every token MintToken returns has already been through splitToken and the
// scan regex, so a token that comes back is one both accepted. What this test
// adds is the negative: an id the parser cannot round-trip is refused before
// anything is printed.
func TestMintRefusesAnIDTheParserCannotRoundTrip(t *testing.T) {
	for _, id := range []string{"", "has space", "has/slash", "has.dot", strings.Repeat("x", 65)} {
		if token, _, err := MintToken(id); err == nil {
			t.Errorf("MintToken(%q) minted %q; an id outside %s must be refused", id, token, TokenIDPattern())
		}
	}
}

// TestMintedTokenAuthenticates closes the loop between the format, the parser
// and authentication: a token this build mints is one this build accepts.
func TestMintedTokenAuthenticates(t *testing.T) {
	token, hash, err := MintToken("tok_admin")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	cfg := configWithCaller(t, "release-bot", "tok_admin", hash, "admin")

	caller, ok := authenticate(cfg, "Bearer "+token)
	if !ok {
		t.Fatalf("a freshly minted token did not authenticate")
	}
	if caller.Name != "release-bot" || caller.TokenID != "tok_admin" || !caller.IsAdmin() {
		t.Errorf("authenticate returned %+v, want caller release-bot / tok_admin / admin", caller)
	}

	// A truncated copy-paste must fail on the checksum rather than look like a
	// wrong secret.
	if _, ok := authenticate(cfg, "Bearer "+token[:len(token)-3]); ok {
		t.Error("a truncated token authenticated")
	}
	// A different secret with a valid-looking shape must fail the hash
	// comparison.
	other, _, err := MintToken("tok_admin")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	if _, ok := authenticate(cfg, "Bearer "+other); ok {
		t.Error("a token with the right id but a different secret authenticated")
	}
}

// TestTokenFieldPatternsDescribeMintedTokens checks the per-field patterns the
// reference publishes against the fields of a real token.
func TestTokenFieldPatternsDescribeMintedTokens(t *testing.T) {
	token, _, err := MintToken("release_bot-2026")
	if err != nil {
		t.Fatalf("MintToken: %v", err)
	}
	prefix, id, secret, checksum := splitForTest(t, token)
	for _, c := range []struct{ field, value, pattern string }{
		{"token_id", id, TokenIDPattern()},
		{"secret", secret, TokenSecretPattern()},
		{"checksum", checksum, TokenChecksumPattern()},
	} {
		if !regexp.MustCompile(c.pattern).MatchString(c.value) {
			t.Errorf("the %s field %q of a minted token does not match its published pattern %s", c.field, c.value, c.pattern)
		}
	}
	if prefix != TokenPrefix() {
		t.Errorf("minted prefix %q, published prefix %q", prefix, TokenPrefix())
	}
}

// splitForTest breaks a token into its four fields, right to left.
func splitForTest(t *testing.T, token string) (prefix, id, secret, checksum string) {
	t.Helper()
	parts := strings.Split(token, "_")
	if len(parts) < 4 {
		t.Fatalf("token %q does not have four fields", token)
	}
	return parts[0], strings.Join(parts[1:len(parts)-2], "_"), parts[len(parts)-2], parts[len(parts)-1]
}

func randomTokenID(t *testing.T) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"
	n := randInt(t, 64) + 1
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[randInt(t, len(alphabet))])
	}
	return b.String()
}

func randInt(t *testing.T, max int) int {
	t.Helper()
	v, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		t.Fatalf("rand: %v", err)
	}
	return int(v.Int64())
}
