package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v90/github"
)

// The installation-token permission set. These three, and only these three.
//
// contents is READ. `contents: write` is never requested — the gateway never
// writes to a workflow file; the operator adds the standby_trigger_id input
// by hand during setup. A future edit that changes this constant is
// caught at construction by New.
const (
	permissionActions  = "write" // dispatching a workflow run
	permissionMetadata = "read"  // repo and workflow listing
	permissionContents = "read"  // reading workflow files for inputs_def
)

// appJWTLifetime is deliberately under GitHub's ceiling, which was bisected on
// live api.github.com on 2026-08-22 and sits at exactly now+600s: exp = now+600
// is accepted, exp = now+660 answers 401 "'Expiration time' claim ('exp') is
// too far in the future". The 60s that 9 minutes leaves under it is the whole
// forward-clock-drift budget — see appJWTBackdate.
const appJWTLifetime = 9 * time.Minute

// appJWTBackdate sets `iat` 60s in the past. It does NOT buy drift margin, and
// the comment that used to stand here credited it with margin that comes from
// somewhere else.
//
// Measured on live api.github.com 2026-08-22: GitHub
// validates `exp` and `iat` against ITS OWN clock, not against each other. A
// token with iat = now-60 and exp = now+600 — a 660-second span — is accepted.
// So the backdate spends none of appJWTLifetime's margin, and it does not widen
// the tolerated drift either. For a gateway clock D AHEAD of GitHub's, `exp`
// binds first and alone:
//
//	exp: appJWTLifetime + D <= 600s          -> D <= 60s
//	iat: D - appJWTBackdate <= ~61s          -> D <= ~121s
//
// Without the backdate the `iat` line would read D <= ~61s, still not the
// binding one. What the backdate actually buys is insurance against GitHub
// tightening `iat` back to zero forward tolerance, which is the case its own
// docs still recommend backdating for. A clock running SLOW is tolerated to
// roughly appJWTLifetime, since exp then lands in the past.
//
// State the size of that budget rather than leaving it to be rediscovered: 60s
// of forward-drift headroom is THIRTY TIMES the 2s clock-sync (NTP) requirement
// the gateway places on its host.
// The gap is not slack to spend — it is the distance between what we require of
// an operator's clock and where GitHub starts refusing us, and it is what makes
// a clock-sync violation show up as drift to be fixed rather than as an outage.
// Raising appJWTLifetime is what would spend it; appjwt_test.go asserts what
// remains is positive.
const appJWTBackdate = 60 * time.Second

// Repo is an owner/name pair.
type Repo struct {
	Owner string
	Name  string
}

// ParseRepo splits "acme/core". It rejects anything else rather than guessing.
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Repo{}, fmt.Errorf("invalid repo %q, want owner/name", s)
	}
	return Repo{Owner: owner, Name: name}, nil
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// identity holds the GitHub App credential and the caches in front of it.
//
// Two caches, two lifetimes:
//   - installation ID per repo, TTL 1h
//   - installation access token per (installation, repo), renewed 5 minutes
//     before expiry
type identity struct {
	cfg     Config
	key     *rsa.PrivateKey
	app     *github.Client
	lim     *limiter
	metrics Metrics
	now     func() time.Time

	jwtMu  sync.Mutex
	jwtVal string
	jwtExp time.Time

	instMu   sync.Mutex
	installs map[string]*installEntry

	tokMu  sync.Mutex
	tokens map[string]*tokenEntry
}

type installEntry struct {
	id        int64
	fetchedAt time.Time
}

type tokenEntry struct {
	// mu serializes minting for one key so a burst of dispatches to the same
	// repo produces one token call, not N.
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read github.private_key_path: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("github.private_key_path %s: no PEM block", path)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github.private_key_path %s: not a PKCS#1 or PKCS#8 key: %w", path, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github.private_key_path %s: not an RSA key", path)
	}
	return key, nil
}

func newIdentity(cfg Config, lim *limiter, m Metrics, httpClient *http.Client) (*identity, error) {
	key, err := loadPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	id := &identity{
		cfg:      cfg,
		key:      key,
		lim:      lim,
		metrics:  m,
		now:      time.Now,
		installs: make(map[string]*installEntry),
		tokens:   make(map[string]*tokenEntry),
	}
	// The App client authenticates with a freshly minted JWT on every request.
	appHTTP := &http.Client{
		Timeout:   cfg.UpstreamTimeout,
		Transport: &jwtTransport{id: id, base: transportOf(httpClient)},
	}
	client, err := newGitHubClient(cfg, appHTTP)
	if err != nil {
		return nil, err
	}
	id.app = client
	return id, nil
}

func transportOf(c *http.Client) http.RoundTripper {
	if c != nil && c.Transport != nil {
		return c.Transport
	}
	return http.DefaultTransport
}

func newGitHubClient(cfg Config, httpClient *http.Client) (*github.Client, error) {
	opts := []github.ClientOptionsFunc{
		github.WithHTTPClient(httpClient),
		github.WithUserAgent(cfg.UserAgent),
	}
	if cfg.BaseURL != "" {
		base := cfg.BaseURL
		opts = append(opts, github.WithURLs(&base, &base))
	}
	return github.NewClient(opts...)
}

// jwtTransport stamps each App-level request with a current App JWT.
type jwtTransport struct {
	id   *identity
	base http.RoundTripper
}

func (t *jwtTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	jwt, err := t.id.appJWT()
	if err != nil {
		return nil, err
	}
	// Per net/http contract: RoundTrip must not modify the request it is given.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+jwt)
	return t.base.RoundTrip(clone)
}

// appJWT returns a cached App JWT, minting a new one shortly before expiry.
func (t *identity) appJWT() (string, error) {
	t.jwtMu.Lock()
	defer t.jwtMu.Unlock()

	now := t.now()
	if t.jwtVal != "" && now.Before(t.jwtExp.Add(-30*time.Second)) {
		return t.jwtVal, nil
	}

	iat := now.Add(-appJWTBackdate)
	exp := now.Add(appJWTLifetime)
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iat": iat.Unix(),
		"exp": exp.Unix(),
		"iss": strconv.FormatInt(t.cfg.AppID, 10),
	}

	signed, err := signRS256(t.key, header, claims)
	if err != nil {
		return "", err
	}
	t.jwtVal, t.jwtExp = signed, exp
	return signed, nil
}

func signRS256(key *rsa.PrivateKey, header map[string]string, claims map[string]any) (string, error) {
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("encode jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode jwt claims: %w", err)
	}
	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(claimsJSON)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}

// githubAppMissingMessage is the 404 body GitHub returns when the App named by
// the JWT's `iss` does not exist. Measured on live api.github.com 2026-08-22: a JWT for an
// unregistered App ID answers 404 {"message":"Integration not found"}, while an
// App that DOES exist but has no installation on the repo answers 404
// {"message":"Not Found"}. Same status, same endpoint; only the body differs.
//
// Compared case-folded, so it is stored folded.
const githubAppMissingMessage = "integration not found"

// appIsMissing reports whether a 404 body names the App ITSELF as the thing
// that was not found.
//
// This is a message-string match, and a message-string match is fragile:
// GitHub can reword a body with no notice and nothing here would fail. It is
// used anyway, for two reasons.
//
// First, the status alone carries no information — matching on it is exactly
// what produced the defect this replaces, where every 404 became "the App is
// not installed on this repo" and the most likely first-setup mistake, a typo
// in github.app_id, was handed the one remedy that cannot fix it.
//
// Second, the fragility is arranged so it can only cost PRECISION, never
// CORRECTNESS:
//
//   - recognised   -> the strong, specific claim (app_id names no App).
//   - unrecognised -> the weak claim, which names the candidates and picks none.
//
// If GitHub rewords the body, a wrong app_id degrades from a precise diagnosis
// to an honest ambiguous one. The old fallback was the confident wrong answer,
// and that is the shape an operator cannot recover from — it sends them to
// /settings/installations to look for an App that does not exist.
//
// The converse does NOT hold and must not be added: a body of "Not Found" is
// not evidence that the App exists and is merely uninstalled. GitHub answers
// the same for a repo that does not exist and for one this App cannot see.
// That half is genuinely undecidable from this response; notFoundOnAppCall says
// so instead of guessing.
//
// If the message match is ever judged too thin to carry the strong claim, the
// independent confirmation is App-level: GET /app answers whether this App
// exists without a repo in scope. It is deliberately NOT called here — it is a
// second upstream request on every 404, in the App bucket, able to fail on its
// own, and adding it is a deliberate design choice rather than something to
// slip in behind a bug fix.
func appIsMissing(message string) bool {
	return strings.Contains(strings.ToLower(message), githubAppMissingMessage)
}

// notFoundOnAppCall converts a 404 taken while authenticating AS THE APP into
// the most specific error the response body actually supports — and no more.
//
// alternatives names the states this particular endpoint's 404 cannot tell
// apart. It is spoken in the error text rather than one of them being chosen,
// because a detail that asserts an unestablished cause is worse than a vague
// one: the vague one leaves the operator looking, the confident wrong one stops
// them looking.
func (t *identity) notFoundOnAppCall(repo Repo, e *Error, cause error, alternatives string) *Error {
	said := ""
	if e.Message != "" {
		said = fmt.Sprintf(" (GitHub said %q)", e.Message)
	}
	if appIsMissing(e.Message) {
		return &Error{
			Kind:           KindAppNotFound,
			UpstreamStatus: e.UpstreamStatus,
			Message: fmt.Sprintf(
				"GitHub App id %d does not exist%s; check github.app_id — installing an App on %s cannot fix this",
				t.cfg.AppID, said, repo),
			Err: cause,
		}
	}
	return &Error{
		Kind:           KindInstallationNotFound,
		UpstreamStatus: e.UpstreamStatus,
		Message: fmt.Sprintf(
			"no installation of app_id %d found for %s%s; %s",
			t.cfg.AppID, repo, said, alternatives),
		Err: cause,
	}
}

// installationID resolves the installation for a repo. The result is
// cached for Config.InstallationTTL.
//
// The call runs on the App JWT, so it is accounted to BucketApp and is not
// subject to the installation quota pause.
func (t *identity) installationID(ctx context.Context, repo Repo) (int64, error) {
	key := repo.String()

	t.instMu.Lock()
	entry, ok := t.installs[key]
	t.instMu.Unlock()
	if ok && t.now().Sub(entry.fetchedAt) < t.cfg.InstallationTTL {
		return entry.id, nil
	}

	release, err := t.lim.acquire(ctx, BucketApp, LaneDispatch)
	if err != nil {
		return 0, err
	}
	inst, resp, err := t.app.Apps.GetRepositoryInstallation(ctx, repo.Owner, repo.Name)
	release()
	t.lim.observe(BucketApp, resp)

	if err != nil {
		e := classify(err, resp)
		// A 404 here is an identity/installation state, not a missing
		// workflow, so it does not stay KindWorkflowNotFound. WHICH identity
		// state it is comes from notFoundOnAppCall reading the body; this
		// branch no longer decides it from the status, which is how a wrong
		// github.app_id came to be reported as an uninstalled App.
		//
		// THE GAUGE CARRIES NO `workflow` LABEL, AND NOT BECAUSE IT WAS
		// OVERLOOKED. Do not add one by inventing a value:
		//
		//  1. There is no workflow in scope. installationID answers
		//     "GET /repos/{o}/{r}/installation" — a repo-scoped endpoint for a
		//     repo-scoped fact — and it is called from every lane, including
		//     Repositories.Get and ListWorkflows, which have no workflow at
		//     all. Any label bound here would be empty on most calls.
		//  2. A gauge is a level, not an event. Labelled by workflow, the
		//     `0` reset below clears only the series of the workflow whose
		//     request happened to succeed; every other workflow of that repo
		//     stays pinned at 1 for the life of the process. The alert would
		//     never recover — worse than the missing dimension.
		//  3. The per-workflow unavailability that DOES exist is a different
		//     condition: a 404 on the workflow itself, which already has
		//     KindWorkflowNotFound and its own mapping. Folding it in here
		//     would conflate two operator states that need different actions
		//     ("install the App" vs "the workflow is gone").
		if e.Kind == KindWorkflowNotFound {
			// The gauge stays at 1 for BOTH identity states, and deliberately.
			// It means "this target is not dispatchable", which is true
			// whether the App is uninstalled or does not exist; when the App
			// does not exist every repo pins to 1, which is also true. Its
			// name carries no cause and the error does — dropping the signal
			// to avoid a misleading name would silence the alert exactly when
			// the gateway is most broken. An App-LEVEL gauge would be the
			// honest addition, and inventing one belongs in the metrics
			// catalog, not at an emission site.
			t.metrics.SetGauge("gateway_target_unavailable", 1, map[string]string{"repo": key})
			return 0, t.notFoundOnAppCall(repo, e, err,
				"the App may not be installed there, or the repo may not exist or "+
					"may not be visible to it — this 404 does not say which")
		}
		return 0, e
	}
	t.metrics.SetGauge("gateway_target_unavailable", 0, map[string]string{"repo": key})

	id := inst.GetID()
	if id == 0 {
		return 0, &Error{Kind: KindUnknown, Err: fmt.Errorf("installation for %s has no id", key)}
	}
	t.instMu.Lock()
	t.installs[key] = &installEntry{id: id, fetchedAt: t.now()}
	t.instMu.Unlock()
	return id, nil
}

// installationPermissions is the installation-token permission request. Repo-scoped, three
// permissions, contents read-only.
func installationPermissions() *github.InstallationPermissions {
	actions, metadata, contents := permissionActions, permissionMetadata, permissionContents
	return &github.InstallationPermissions{
		Actions:  &actions,
		Metadata: &metadata,
		Contents: &contents,
	}
}

// token returns a repo-scoped installation access token, minting or renewing
// as needed.
//
// Degradation rule: if renewal fails while the current token
// is still valid, the current token is used. Only once it has actually expired
// does the caller get KindCredentialExpired — which the HTTP layer turns into
// 503 credential_expired. The failure is never hidden behind a fresh attempt
// that silently succeeds later.
func (t *identity) token(ctx context.Context, repo Repo) (string, int64, error) {
	instID, err := t.installationID(ctx, repo)
	if err != nil {
		return "", 0, err
	}
	key := strconv.FormatInt(instID, 10) + "/" + repo.String()

	t.tokMu.Lock()
	entry, ok := t.tokens[key]
	if !ok {
		entry = &tokenEntry{}
		t.tokens[key] = entry
	}
	t.tokMu.Unlock()

	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := t.now()
	if entry.token != "" && now.Before(entry.expiresAt.Add(-t.cfg.TokenRefreshLead)) {
		return entry.token, instID, nil
	}

	release, err := t.lim.acquire(ctx, BucketApp, LaneDispatch)
	if err != nil {
		return "", 0, err
	}
	tok, resp, err := t.app.Apps.CreateInstallationToken(ctx, instID, &github.InstallationTokenOptions{
		// Repo-scoped on every mint. This is the only mitigation that
		// constrains an attacker on GitHub's side.
		Repositories: []string{repo.Name},
		Permissions:  installationPermissions(),
	})
	release()
	t.lim.observe(BucketApp, resp)

	if err != nil {
		if entry.token != "" && now.Before(entry.expiresAt) {
			t.metrics.IncCounter("gateway_github_token_refresh_failed_total",
				map[string]string{"repo": repo.String()})
			return entry.token, instID, nil
		}
		e := classify(err, resp)
		if e.Kind == KindWorkflowNotFound {
			// Same body-versus-status rule as installationID. The alternatives
			// differ: installationID already answered for this repo, so the
			// installation id existed at least as of the cached lookup and the
			// interesting cases are removal and App deletion since.
			return "", 0, t.notFoundOnAppCall(repo, e, err,
				fmt.Sprintf("installation %d answered for this repo when it was cached, "+
					"so it may have been removed since — this 404 does not say which", instID))
		}
		return "", 0, &Error{Kind: KindCredentialExpired, UpstreamStatus: e.UpstreamStatus, Err: err}
	}

	if tok.GetToken() == "" {
		return "", 0, &Error{Kind: KindCredentialExpired, Err: errors.New("empty installation token")}
	}
	entry.token = tok.GetToken()
	entry.expiresAt = tok.GetExpiresAt().Time
	if entry.expiresAt.IsZero() {
		// GitHub documents one hour. Without an expiry we cannot honour the
		// renewal lead, so assume the documented lifetime rather than treat
		// the token as immortal.
		entry.expiresAt = now.Add(time.Hour)
	}
	return entry.token, instID, nil
}

// invalidate drops the cached token for a repo. Called when GitHub rejects it
// with 401, so the next call mints a fresh one instead of looping on a token
// that is dead early (for example after the App's credentials were rotated).
func (t *identity) invalidate(instID int64, repo Repo) {
	key := strconv.FormatInt(instID, 10) + "/" + repo.String()
	t.tokMu.Lock()
	entry, ok := t.tokens[key]
	t.tokMu.Unlock()
	if !ok {
		return
	}
	entry.mu.Lock()
	entry.token = ""
	entry.expiresAt = time.Time{}
	entry.mu.Unlock()
}
