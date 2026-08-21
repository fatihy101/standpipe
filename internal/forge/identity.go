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

// appJWTLifetime is deliberately under GitHub's 10-minute ceiling.
const appJWTLifetime = 9 * time.Minute

// appJWTBackdate absorbs small clock drift. The gateway requires NTP with at
// most 2s drift; 60s is the conventional margin GitHub's own docs suggest.
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
		// A 404 here means the App is not installed on this repo. That
		// is a distinct operator-visible state, not a missing workflow.
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
			t.metrics.SetGauge("gateway_target_unavailable", 1, map[string]string{"repo": key})
			return 0, &Error{
				Kind:           KindInstallationNotFound,
				UpstreamStatus: e.UpstreamStatus,
				Message:        fmt.Sprintf("GitHub App is not installed on %s", key),
				Err:            err,
			}
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
			return "", 0, &Error{
				Kind:           KindInstallationNotFound,
				UpstreamStatus: e.UpstreamStatus,
				Message:        fmt.Sprintf("GitHub App is not installed on %s", repo),
				Err:            err,
			}
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
