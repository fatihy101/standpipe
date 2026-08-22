package forge

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/testutil"
)

// The App JWT had NO test of any kind before this file, and the fake GitHub
// never reads the Authorization header, so every test in the suite passes with
// a JWT that GitHub would refuse. What follows pins the minter against limits
// MEASURED on live api.github.com on 2026-08-22, not against the fake.
//
// How the measurement was possible without a registered App: GitHub checks the
// `exp`/`iat` claims BEFORE it checks whether the issuer exists. A JWT for an
// App ID that does not exist answers 404 "Integration not found" when the
// claims are legal and 401 "'Expiration time' claim ('exp') is too far in the
// future" when they are not, so the boundary is observable from an unregistered
// key. Bisected against https://api.github.com/app:
//
//	exp = now + 600  -> claims accepted   exp = now + 660 -> 401 exp too far
//	iat = now +  61  -> claims accepted   iat = now +  90 -> 401 Bad credentials
//
// Both ceilings are measured against GITHUB'S clock and not against `iat`: a
// token with iat = now-60 and exp = now+600 (a 660-second span) is accepted,
// so appJWTBackdate does not spend any of appJWTLifetime's margin.
const (
	// measuredExpCeiling is the largest exp-minus-now GitHub accepted.
	measuredExpCeiling = 600 * time.Second
	// measuredIatFutureTolerance is the largest iat-minus-now GitHub accepted.
	measuredIatFutureTolerance = 61 * time.Second
)

func TestAppJWTFitsMeasuredGitHubClaimLimits(t *testing.T) {
	e := newEnv(t, nil)
	e.c.id.now = e.clock.Now

	token, err := e.c.id.appJWT()
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}
	hdr, claims := decodeJWT(t, token)

	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
		t.Fatalf("header = %v, want alg RS256 typ JWT", hdr)
	}
	// The issuer is the App ID. It is sent as a decimal STRING; GitHub
	// accepts either, but a change here is a change to what GitHub parses.
	if got := claims["iss"]; got != "12345" {
		t.Fatalf("iss = %v, want \"12345\"", got)
	}

	now := e.clock.Now()
	iat := claimTime(t, claims, "iat")
	exp := claimTime(t, claims, "exp")

	if d := exp.Sub(now); d > measuredExpCeiling {
		t.Fatalf("exp is now+%s; live GitHub refuses anything past now+%s "+
			"(401 'Expiration time' claim ('exp') is too far in the future). "+
			"appJWTLifetime is %s", d, measuredExpCeiling, appJWTLifetime)
	}
	if d := iat.Sub(now); d > measuredIatFutureTolerance {
		t.Fatalf("iat is now+%s; live GitHub refuses a future iat past now+%s",
			d, measuredIatFutureTolerance)
	}
	if !iat.Before(exp) {
		t.Fatalf("iat %s is not before exp %s", iat, exp)
	}

	// The exp ceiling is what bounds tolerable FORWARD clock drift: a gateway
	// clock D ahead of GitHub's mints exp = githubNow+D+appJWTLifetime, which
	// must stay under the ceiling. Stated as an assertion so raising
	// appJWTLifetime cannot silently spend the margin the 2s clock-sync requirement relies on.
	if drift := measuredExpCeiling - appJWTLifetime; drift <= 0 {
		t.Fatalf("appJWTLifetime %s leaves no forward-drift margin under the "+
			"measured %s ceiling", appJWTLifetime, measuredExpCeiling)
	}
}

func TestAppJWTSignatureVerifiesAgainstThePrivateKey(t *testing.T) {
	e := newEnv(t, nil)
	e.c.id.now = e.clock.Now

	token, err := e.c.id.appJWT()
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&e.c.id.key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("RS256 signature does not verify over header.claims: %v", err)
	}
}

func TestAppJWTIsCachedAndRenewedBeforeExpiry(t *testing.T) {
	e := newEnv(t, nil)
	e.c.id.now = e.clock.Now

	first, err := e.c.id.appJWT()
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}
	again, err := e.c.id.appJWT()
	if err != nil {
		t.Fatalf("appJWT: %v", err)
	}
	if again != first {
		t.Fatal("a second call minted a new JWT; the cache is not being used")
	}

	// The minter renews 30s before expiry. Land inside that window.
	e.clock.Advance(appJWTLifetime - 15*time.Second)
	renewed, err := e.c.id.appJWT()
	if err != nil {
		t.Fatalf("appJWT after advance: %v", err)
	}
	if renewed == first {
		t.Fatal("the JWT was not renewed inside the 30s pre-expiry window")
	}
	_, claims := decodeJWT(t, renewed)
	if d := claimTime(t, claims, "exp").Sub(e.clock.Now()); d > measuredExpCeiling {
		t.Fatalf("renewed exp is now+%s, past the measured %s ceiling", d, measuredExpCeiling)
	}
}

// loadPrivateKey is what forge.New calls at construction. These pin the two
// shapes that reach an operator: a file that is not a PEM at all, and a PEM
// that decodes but is not an RSA key. Both were measured to be fatal at
// `standpiped` startup on 2026-08-22 — and to be accepted silently by
// `standpipe config validate`, which only stats the file.
func TestLoadPrivateKeyRejectsCorruptMaterial(t *testing.T) {
	dir := t.TempDir()
	keyPath, err := testutil.WriteAppKey(dir)
	if err != nil {
		t.Fatalf("WriteAppKey: %v", err)
	}
	good, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key: %v", err)
	}

	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"not a pem", []byte("this is not a key\n"), "no PEM block"},
		{"truncated pem", good[:len(good)/3], "no PEM block"},
		{"pem block with a mutated body", mutatePEMBody(t, good), "not a PKCS#1 or PKCS#8 key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := dir + "/" + strings.ReplaceAll(tc.name, " ", "-") + ".pem"
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := loadPrivateKey(path)
			if err == nil {
				t.Fatal("corrupt key accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// --- helpers ----------------------------------------------------------------

// mutatePEMBody keeps the PEM armour intact and corrupts the DER inside, which
// is what a truncated download or a mangled copy-paste actually produces.
func mutatePEMBody(t *testing.T, good []byte) []byte {
	t.Helper()
	block, _ := pem.Decode(good)
	if block == nil {
		t.Fatal("fixture key is not a PEM")
	}
	der := append([]byte(nil), block.Bytes...)
	for i := 1; i < len(der) && i < 8; i++ {
		der[i] ^= 0xff
	}
	return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: der})
}

func decodeJWT(t *testing.T, token string) (map[string]any, map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}
	return decodeJWTSegment(t, parts[0]), decodeJWTSegment(t, parts[1])
}

func decodeJWTSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not base64url: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
	return out
}

func claimTime(t *testing.T, claims map[string]any, name string) time.Time {
	t.Helper()
	v, ok := claims[name].(float64)
	if !ok {
		t.Fatalf("claim %q is %T, want a number", name, claims[name])
	}
	return time.Unix(int64(v), 0)
}
