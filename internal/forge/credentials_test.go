package forge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// THE HALF OF THE GATEWAY THAT HAS NEVER MET REAL GITHUB.
//
// The setup guide (docs/setup.md) says it plainly: the 2026-08-21 measurements were made with curl
// against api.github.com, NOT through the gateway, so "App JWT minting,
// installation-token minting and renewal, the per-installation quota bucket,
// and the injection -> matching chain" have only ever run against the fake.
//
// The fake DOES NOT READ the Authorization header (that is deliberate — see
// testutil.GitHub.Authorizations), so until this file existed every test in
// the suite passed with a JWT that GitHub would refuse, and with credentials
// attached to the wrong endpoints. internal/forge/appjwt_test.go closed half
// of it by testing the MINTER directly. This closes the other half: what
// actually goes on the WIRE, and on which endpoint.
//
// Real GitHub distinguishes two credential CLASSES and 401s when they are
// swapped:
//
//	App JWT             GET  /repos/{o}/{r}/installation
//	                    POST /app/installations/{id}/access_tokens
//	installation token  everything else
//
// A gateway that sent the JWT to the dispatch endpoint would work perfectly
// here and fail on the operator's first real call — during setup, in their
// terminal, which is the worst possible place to find it.
// ---------------------------------------------------------------------------

// appJWTEndpoints are the operations real GitHub authenticates with the App
// JWT. installationTokenEndpoints are the rest.
var (
	appJWTEndpoints = []testutil.Operation{
		testutil.OpInstallation,
		testutil.OpAccessToken,
	}
	installationTokenEndpoints = []testutil.Operation{
		testutil.OpGetRepo,
		testutil.OpListWorkflow,
		testutil.OpGetContents,
		testutil.OpDispatch,
		testutil.OpListRuns,
	}
)

// exerciseEveryEndpoint drives one repo through discovery, dispatch and a
// resolver pass so that all seven operations have been called at least once.
func exerciseEveryEndpoint(t *testing.T, e *env) {
	t.Helper()
	e.seed("acme/core", "release.yml", workflowYAML)
	_, at := e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_cred")

	r := NewResolver(e.c)
	_, err := r.ResolveOne(context.Background(),
		PendingTrigger{
			EventID: "evt_cred", Repo: "acme/core", Workflow: "release.yml",
			Ref: "refs/heads/main", At: at, Correlation: CorrelationInjected,
		},
		ResolveWindow{AttemptAt: at, DispatchAt: &at, OriginalEventID: "evt_cred"})
	if err != nil {
		t.Fatalf("ResolveOne: %v", err)
	}

	for _, op := range append(append([]testutil.Operation{}, appJWTEndpoints...), installationTokenEndpoints...) {
		if e.gh.Calls(op) == 0 {
			t.Fatalf("operation %q was never called; this test proves nothing about it", op)
		}
	}
}

func TestEachEndpointGetsTheCredentialClassGitHubRequires(t *testing.T) {
	e := newEnv(t, nil)
	e.c.id.now = e.clock.Now
	exerciseEveryEndpoint(t, e)

	for _, op := range appJWTEndpoints {
		for i, header := range e.gh.Authorizations(op) {
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok {
				t.Fatalf("%s call %d sent Authorization %q; GitHub's App endpoints take "+
					"`Bearer <JWT>`", op, i, header)
			}
			if strings.HasPrefix(token, "ghs_") {
				t.Fatalf("%s call %d was authenticated with an INSTALLATION TOKEN. Real GitHub "+
					"answers 401 to that; the fake does not read the header, so nothing else in "+
					"the suite would notice", op, i)
			}
			hdr, claims := decodeJWT(t, token)
			if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" {
				t.Fatalf("%s call %d JWT header = %v, want alg RS256 typ JWT", op, i, hdr)
			}
			if claims["iss"] != "12345" {
				t.Fatalf("%s call %d iss = %v, want the App ID \"12345\"", op, i, claims["iss"])
			}
			// The measured ceilings, applied to what actually travelled
			// rather than to what the minter returned. A cached JWT that
			// outlives its window would pass appjwt_test and fail here.
			now := e.clock.Now()
			if d := claimTime(t, claims, "exp").Sub(now); d > measuredExpCeiling {
				t.Fatalf("%s call %d sent exp = now+%s; live GitHub refuses past now+%s",
					op, i, d, measuredExpCeiling)
			}
			if d := claimTime(t, claims, "iat").Sub(now); d > measuredIatFutureTolerance {
				t.Fatalf("%s call %d sent iat = now+%s; live GitHub refuses past now+%s",
					op, i, d, measuredIatFutureTolerance)
			}
		}
	}

	// `ghs_` is GitHub's own prefix for an installation access token, and the
	// fake mints exactly one value (ghs_fake_installation_token) for this
	// test's single installation. Requiring the prefix AND that every repo
	// endpoint carried the same value proves the client used the mint's
	// result rather than falling back to the JWT, to a stale value, or to no
	// credential at all — without pinning the fake's literal.
	var used string
	for _, op := range installationTokenEndpoints {
		for i, header := range e.gh.Authorizations(op) {
			token, ok := strings.CutPrefix(header, "Bearer ")
			if !ok || !strings.HasPrefix(token, "ghs_") {
				t.Fatalf("%s call %d sent Authorization %q, want the `Bearer ghs_...` installation "+
					"token. GitHub's repo endpoints answer 401 Bad credentials to an App JWT, and "+
					"the fake reads no header, so nothing else here would notice", op, i, header)
			}
			if used == "" {
				used = token
			} else if token != used {
				t.Fatalf("%s call %d used token %q while an earlier call used %q; one installation "+
					"mints one token and the cache is per installation+repo", op, i, token, used)
			}
		}
	}
}

func TestTheInstallationTokenIsMintedOnceAndRenewedBeforeExpiry(t *testing.T) {
	// Renewal before expiry. The fake's access-token endpoint answers
	// expires_at = its clock + 1h, so this test only means anything with the
	// identity's clock pointed at the SAME clock the fake uses.
	//
	// IT IS NOT POINTED THERE BY newEnv. Without the line below, id.now is the
	// real wall clock, the fake's expires_at is in the fake's timeline, every
	// token reads as long expired, and the gateway mints a fresh installation
	// token FOR EVERY SINGLE API CALL — four mints for one seed-and-dispatch.
	// That is a property of the harness, not of the product, but it is also
	// the reason no test had ever exercised the cache.
	e := newEnv(t, nil)
	e.c.id.now = e.clock.Now
	e.seed("acme/core", "release.yml", workflowYAML)

	if got := e.gh.Calls(testutil.OpAccessToken); got != 1 {
		t.Fatalf("discovery minted %d installation tokens, want 1 — the token is cached per "+
			"installation+repo", got)
	}

	e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_1")
	if got := e.gh.Calls(testutil.OpAccessToken); got != 1 {
		t.Fatalf("a dispatch inside the token's life minted again (%d total); the cache is not "+
			"being used and every request would spend an App-bucket call", got)
	}

	// Just inside the renewal lead: still no new mint. expires_at is
	// BaseTime+1h and DefaultTokenRefreshLead is 5m, so BaseTime+54m is
	// comfortably inside.
	e.clock.Advance(54 * time.Minute)
	e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_2")
	if got := e.gh.Calls(testutil.OpAccessToken); got != 1 {
		t.Fatalf("a token %s from expiry was renewed early (%d mints); the refresh lead is %s",
			6*time.Minute, got, DefaultTokenRefreshLead)
	}

	// Inside the refresh lead: renewed BEFORE it expires, which is the whole point —
	// a token that is renewed only on expiry has already failed a request.
	e.clock.Advance(2 * time.Minute)
	e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_3")
	if got := e.gh.Calls(testutil.OpAccessToken); got != 2 {
		t.Fatalf("mints = %d, want 2: a token %s from expiry is inside the %s renewal lead",
			got, 4*time.Minute, DefaultTokenRefreshLead)
	}
}

func TestARejectedInstallationTokenIsDroppedRatherThanReused(t *testing.T) {
	// Token rotation. A 401 on a repo endpoint means the installation
	// token is dead early — before its expires_at. If the cache kept it, every
	// subsequent call would fail with the same credential until the hour ran
	// out, and the operator would see 503 credential_expired for an hour after
	// a rotation that took a second.
	e := newEnv(t, nil)
	e.c.id.now = e.clock.Now
	e.seed("acme/core", "release.yml", workflowYAML)

	minted := e.gh.Calls(testutil.OpAccessToken)
	if minted != 1 {
		t.Fatalf("setup minted %d tokens, want 1", minted)
	}

	e.gh.FailNext(testutil.OpDispatch, testutil.FailUnauthorized, 1)
	r, err := ParseRepo("acme/core")
	if err != nil {
		t.Fatalf("ParseRepo: %v", err)
	}
	req := DispatchRequest{
		Repo: r, Workflow: "release.yml", Ref: "refs/heads/main",
		Inputs: map[string]string{}, TriggerEventID: "evt_401",
	}
	plan, err := e.c.Plan(context.Background(), req)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := e.c.Dispatch(context.Background(), req, plan); err == nil {
		t.Fatal("the dispatch succeeded against an injected 401")
	} else if kind := KindOf(err); kind != KindCredentialExpired {
		t.Fatalf("Kind = %q, want %q — a rejected installation token is the credential path, "+
			"not a generic upstream failure", kind, KindCredentialExpired)
	}

	// The next dispatch must NOT reuse the rejected token.
	e.dispatchOnce("acme/core", "release.yml", "refs/heads/main", "evt_after")
	if got := e.gh.Calls(testutil.OpAccessToken); got != minted+1 {
		t.Fatalf("mints = %d, want %d: the 401'd token was reused instead of being dropped "+
			"(identity.invalidate, client.go)", got, minted+1)
	}
}
