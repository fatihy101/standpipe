package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Errors is every validation failure at once. Reporting one per run turns
// fixing a config file into a guessing loop.
type Errors []error

func (e Errors) Error() string {
	msgs := make([]string, 0, len(e))
	for _, err := range e {
		msgs = append(msgs, err.Error())
	}
	return "config: " + strings.Join(msgs, "\n        ")
}

func (e Errors) Unwrap() []error { return e }

var (
	repoRE      = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	workflowRE  = regexp.MustCompile(`^[A-Za-z0-9._-]+\.(yml|yaml)$`)
	tokenHashRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tokenIDRE   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	callerRE    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Validate reports everything wrong with a defaulted configuration.
//
// It is the whole of `standpipe config validate` and it is also what
// gateway startup and SIGHUP run. There is exactly one implementation on
// purpose: a file that validates from the CLI and then fails to load in the
// gateway would make `config validate` worse than useless.
func (c *Config) Validate() error {
	var errs Errors

	// --- gateway ---
	if c.Gateway.Listen == "" {
		errs = append(errs, errors.New("gateway.listen is required"))
	}
	if c.Gateway.AdminListen == "" {
		// The admin surface binds SEPARATELY. Falling back to the public
		// listener would publish /metrics and /inventory to the internet.
		errs = append(errs, errors.New("gateway.admin_listen is required; the admin surface never shares the public listener"))
	}
	if c.Gateway.Listen == c.Gateway.AdminListen {
		errs = append(errs, errors.New("gateway.admin_listen must differ from gateway.listen"))
	}
	if c.Gateway.MaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("gateway.max_body_bytes must be positive, got %d", c.Gateway.MaxBodyBytes))
	}
	if c.Gateway.IdempotencyTTL.D() <= 0 {
		errs = append(errs, errors.New("gateway.idempotency_ttl must be positive"))
	}
	if c.Gateway.UpstreamTimeout.D() <= 0 {
		errs = append(errs, errors.New("gateway.upstream_timeout must be positive"))
	}
	if c.Gateway.ShutdownGrace.D() <= c.Gateway.UpstreamTimeout.D() {
		// The grace is upstream_timeout + margin. A shorter grace kills
		// dispatches that were still legally in flight, and their idempotency
		// rows are left for the startup sweep to reopen as `unknown`.
		errs = append(errs, fmt.Errorf("gateway.shutdown_grace (%s) must exceed gateway.upstream_timeout (%s)",
			c.Gateway.ShutdownGrace.D(), c.Gateway.UpstreamTimeout.D()))
	}
	if c.Gateway.RetentionDays <= 0 {
		errs = append(errs, fmt.Errorf("gateway.retention_days must be positive, got %d", c.Gateway.RetentionDays))
	}
	for i, cidr := range c.Gateway.TrustedProxies {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			errs = append(errs, fmt.Errorf("gateway.trusted_proxies[%d]: %q is not a CIDR block: %v", i, cidr, err))
			continue
		}
		if p != p.Masked() {
			// Called out by name: 127.0.0.1/8 is not 127.0.0.0/8.
			// Go accepts host bits and silently masks them, so a typo here
			// would trust a wider range than the operator wrote.
			errs = append(errs, fmt.Errorf("gateway.trusted_proxies[%d]: %q has host bits set; write %q", i, cidr, p.Masked().String()))
		}
	}
	errs = append(errs, validateBucket("gateway.rate_limit.per_caller", c.Gateway.RateLimit.PerCaller)...)
	errs = append(errs, validateBucket("gateway.rate_limit.global", c.Gateway.RateLimit.Global)...)
	errs = append(errs, validateBucket("gateway.rate_limit.pre_auth_ip", c.Gateway.RateLimit.PreAuthIP)...)
	if c.Gateway.RateLimit.RejectLogWindow.D() <= 0 {
		errs = append(errs, errors.New("gateway.rate_limit.reject_log_window must be positive"))
	}

	// --- storage ---
	if c.Storage.SQLitePath == "" {
		errs = append(errs, errors.New("storage.sqlite_path is required"))
	}

	// --- github ---
	if c.GitHub.AppID <= 0 {
		errs = append(errs, errors.New("github.app_id is required and must be positive"))
	}
	if c.GitHub.PrivateKeyPath == "" {
		errs = append(errs, errors.New("github.private_key_path is required"))
	}
	if c.GitHub.MaxInflight < 1 {
		errs = append(errs, fmt.Errorf("github.max_inflight must be >= 1, got %d", c.GitHub.MaxInflight))
	}
	if c.GitHub.MaxQueueWait.D() <= 0 {
		errs = append(errs, errors.New("github.max_queue_wait must be positive"))
	}
	// github.base_url is defaulted before validation, so an empty value here
	// means applyDefaults was bypassed. It is checked rather than assumed:
	// go-github rejects a bad root at client construction, which would surface
	// as a startup failure with the operator's typo buried in a library error.
	if err := validateGitHubBaseURL(c.GitHub.BaseURL); err != nil {
		errs = append(errs, err)
	}

	// --- allowed_targets ---
	if len(c.AllowedTargets) == 0 {
		// An empty server-side allowlist means every request is 403.
		// That is a working gateway that answers nothing, which is worse than
		// a refusal to start.
		errs = append(errs, errors.New("allowed_targets is empty; no request could ever be authorized"))
	}
	seenTarget := map[string]bool{}
	for i, t := range c.AllowedTargets {
		where := fmt.Sprintf("allowed_targets[%d]", i)
		if !repoRE.MatchString(t.Repo) {
			errs = append(errs, fmt.Errorf("%s.repo: %q is not owner/name", where, t.Repo))
		}
		if seenTarget[t.Repo] {
			errs = append(errs, fmt.Errorf("%s.repo: %q appears more than once; merge the entries", where, t.Repo))
		}
		seenTarget[t.Repo] = true
		if len(t.Workflows) == 0 {
			errs = append(errs, fmt.Errorf("%s.workflows is empty", where))
		}
		for j, w := range t.Workflows {
			if !workflowRE.MatchString(w) {
				errs = append(errs, fmt.Errorf("%s.workflows[%d]: %q must be a workflow file name such as release.yml", where, j, w))
			}
		}
		// An absent refs list is legal and means "default branch only".
		// It is not defaulted to a literal here: the default branch
		// is a per-repo fact that lives in workflow_cache, and a request
		// against a target whose default branch is not known yet is a 403
		// default_branch_unknown, not a guess.
		for j, ref := range t.Refs {
			if err := validateRef(ref); err != nil {
				errs = append(errs, fmt.Errorf("%s.refs[%d]: %v", where, j, err))
			}
		}
	}

	// --- callers ---
	if len(c.Callers) == 0 {
		errs = append(errs, errors.New("callers is empty; no request could ever authenticate"))
	}
	tokenIDs := map[string]string{}
	hashes := map[string]string{}
	for _, name := range sortedCallerNames(c.Callers) {
		caller := c.Callers[name]
		where := "callers." + name
		if !callerRE.MatchString(name) {
			errs = append(errs, fmt.Errorf("%s: caller name must match %s; it is the idempotency namespace", where, callerRE))
		}
		if !tokenHashRE.MatchString(caller.TokenHash) {
			errs = append(errs, fmt.Errorf("%s.token_hash must be sha256:<64 lowercase hex>; the plaintext token is never stored", where))
		}
		if !tokenIDRE.MatchString(caller.TokenID) {
			errs = append(errs, fmt.Errorf("%s.token_id is required and must match %s", where, tokenIDRE))
		}
		if prev, ok := tokenIDs[caller.TokenID]; ok {
			// token_id is what authentication looks a caller up by. Two callers
			// sharing one makes the lookup non-deterministic, which decides
			// authorization.
			errs = append(errs, fmt.Errorf("%s.token_id %q is already used by callers.%s", where, caller.TokenID, prev))
		}
		tokenIDs[caller.TokenID] = name
		if prev, ok := hashes[caller.TokenHash]; ok && caller.TokenHash != "" {
			errs = append(errs, fmt.Errorf("%s.token_hash is identical to callers.%s; two callers cannot share one token", where, prev))
		}
		hashes[caller.TokenHash] = name

		switch caller.Role {
		case RoleCaller, RoleAdmin:
		default:
			errs = append(errs, fmt.Errorf("%s.role must be %q or %q, got %q", where, RoleCaller, RoleAdmin, caller.Role))
		}
		if caller.Role == RoleCaller && len(caller.Allow) == 0 {
			errs = append(errs, fmt.Errorf("%s.allow is empty; this caller can trigger nothing", where))
		}
		for j, a := range caller.Allow {
			aw := fmt.Sprintf("%s.allow[%d]", where, j)
			if !repoRE.MatchString(a.Repo) {
				errs = append(errs, fmt.Errorf("%s.repo: %q is not owner/name", aw, a.Repo))
			}
			if !workflowRE.MatchString(a.Workflow) {
				errs = append(errs, fmt.Errorf("%s.workflow: %q must be a workflow file name; no wildcard is allowed here", aw, a.Workflow))
			}
			if err := validateRef(a.Ref); err != nil {
				errs = append(errs, fmt.Errorf("%s.ref: %v", aw, err))
			}
			if !targetAllowed(c.AllowedTargets, a) {
				// Not fatal on its own — but a caller policy that names a
				// target the server-side allowlist does not carry is dead
				// configuration that reads as working (allowed_targets always wins).
				errs = append(errs, fmt.Errorf("%s: %s:%s is not in allowed_targets, so this entry can never authorize anything", aw, a.Repo, a.Workflow))
			}
		}
	}

	// --- inventory ---
	if c.Inventory.HistoryDays <= 0 {
		errs = append(errs, fmt.Errorf("inventory.history_days must be positive, got %d", c.Inventory.HistoryDays))
	}
	if c.Inventory.RefreshInterval.D() <= 0 {
		errs = append(errs, errors.New("inventory.refresh_interval must be positive"))
	}

	// --- observability ---
	switch c.Observability.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("observability.log_level must be debug, info, warn or error, got %q", c.Observability.LogLevel))
	}
	switch c.Observability.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("observability.log_format must be json or text, got %q", c.Observability.LogFormat))
	}
	for i, ip := range c.Observability.WatchIPs {
		if _, err := netip.ParseAddr(ip); err != nil {
			errs = append(errs, fmt.Errorf("observability.watch_ips[%d]: %q is not an IP address", i, ip))
		}
	}

	// --- clock ---
	if c.Clock.MaxDriftSeconds < 0 {
		errs = append(errs, errors.New("clock.max_drift_seconds cannot be negative"))
	}

	if len(errs) == 0 {
		return nil
	}
	return errs
}

// CheckFiles reports files the configuration names that cannot be read. It is
// separate from Validate because `standpipe config validate` is routinely run
// from a workstation where the App private key does not exist; the file being
// absent there says nothing about the file being absent on the gateway host.
// Startup calls it and treats the result as fatal; the CLI reports it as a
// warning.
func (c *Config) CheckFiles() error {
	var errs Errors
	if c.GitHub.PrivateKeyPath != "" {
		if _, err := os.Stat(c.GitHub.PrivateKeyPath); err != nil {
			errs = append(errs, fmt.Errorf("github.private_key_path: %v", err))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func validateBucket(where string, b Bucket) []error {
	var errs []error
	if b.RatePerSecond <= 0 {
		errs = append(errs, fmt.Errorf("%s.rate_per_second must be positive, got %v", where, b.RatePerSecond))
	}
	if b.Burst < 1 {
		errs = append(errs, fmt.Errorf("%s.burst must be >= 1, got %d", where, b.Burst))
	}
	return errs
}

// validateRef accepts "*" or a full ref path. A short name such as
// "main" is not accepted, so a policy written with one would silently never
// match the request it was meant to allow.
func validateRef(ref string) error {
	switch {
	case ref == "":
		return errors.New("a ref is required; use \"*\" to allow every ref")
	case ref == "*":
		return nil
	case strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/"):
		return globCheck(ref)
	case strings.HasPrefix(ref, "refs/tags/") && len(ref) > len("refs/tags/"):
		return globCheck(ref)
	default:
		return fmt.Errorf("%q must be a full ref (refs/heads/... or refs/tags/...) or \"*\"; a short name is not accepted", ref)
	}
}

// globCheck enforces the allowlist glob semantics at validation time: `*` matches
// within one path segment and `**` is not supported. Accepting `**` here and
// then not implementing it would produce a pattern that matches nothing.
func globCheck(ref string) error {
	if strings.Contains(ref, "**") {
		return fmt.Errorf("%q: ** is not supported; * matches within one path segment only", ref)
	}
	if i := strings.Index(ref, "*"); i >= 0 && strings.Contains(ref[i:], "/") {
		return fmt.Errorf("%q: * is only allowed in the last path segment", ref)
	}
	return nil
}

func targetAllowed(targets []AllowedTarget, a Allow) bool {
	for _, t := range targets {
		if t.Repo != a.Repo {
			continue
		}
		for _, w := range t.Workflows {
			if w == a.Workflow {
				return true
			}
		}
	}
	return false
}

func sortedCallerNames(m map[string]Caller) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validateGitHubBaseURL checks `github.base_url`.
//
// It must be an absolute http/https URL with a host and no query or fragment.
// A relative value, or one carrying a query string, produces a client that
// builds request paths nobody intended — and the first symptom would be a 404
// from GitHub that reads like a permissions problem.
func validateGitHubBaseURL(raw string) error {
	if raw == "" {
		return errors.New("github.base_url is empty; it defaults to " + DefaultGitHubBaseURL + " and must never be blank")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("github.base_url %q is not a URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("github.base_url %q must be an absolute http:// or https:// URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("github.base_url %q has no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("github.base_url %q must not carry a query string or fragment", raw)
	}
	return nil
}
