package httpapi

import (
	"encoding/json"
)

// This file exposes the closed lists this package owns so that
// internal/docgen can render them into docs/reference/tokens.md and
// docs/reference/health.md. The code is the source of truth; the Markdown is
// generated from it and a test fails when the two disagree.

// TokenPrefix returns the fixed prefix every caller token starts with.
func TokenPrefix() string { return tokenPrefix }

// TokenScanPattern returns the secret-scanning regex for caller tokens. It is
// the exact pattern MintToken enforces on its own output before it emits a
// token, so a token this build mints is always one the published pattern
// matches in full.
func TokenScanPattern() string { return tokenScanRE.String() }

// TokenIDPattern returns the anchored pattern a token_id must match.
func TokenIDPattern() string { return tokenIDRE.String() }

// TokenSecretPattern returns the anchored pattern of the secret field.
func TokenSecretPattern() string { return tokenSecretRE.String() }

// TokenChecksumPattern returns the anchored pattern of the checksum field.
func TokenChecksumPattern() string { return tokenChecksumRE.String() }

// HealthField documents one key of the admin listener's detailed /healthz
// body.
type HealthField struct {
	// Name is the JSON key.
	Name string
	// Meaning says what the value reports.
	Meaning string
	// Presence says when the key is written and when its value can be
	// trusted.
	Presence string
	// MayBeAbsent is true when the key itself can be missing from the body.
	// For these fields the absence carries meaning of its own; for every
	// other field the key is always written, even when its value is only
	// meaningful under a condition.
	MayBeAbsent bool
}

// HealthFields is the closed list of keys the detailed /healthz body can
// carry, in the order the body writes them. A test marshals adminHealth and
// fails if this list and the encoder disagree in either direction, or about
// which keys can be absent.
func HealthFields() []HealthField {
	return []HealthField{
		{Name: "status", Meaning: "`ok` or `degraded`; `degraded` only while `sqlite_ok` is false.", Presence: "always"},
		{Name: "version", Meaning: "The binary's version.", Presence: "always"},
		{Name: "uptime_seconds", Meaning: "Seconds since the process started.", Presence: "always"},
		{Name: "sqlite_ok", Meaning: "SQLite `quick_check` passed **and** free space is above `storage.min_free_bytes`.", Presence: "always"},
		{Name: "schema_version", Meaning: "The schema version stored in the database.", Presence: "always (0 when it cannot be read)"},
		{Name: "github_reachable", Meaning: "The most recent GitHub call the gateway classified produced an HTTP **response**. It says nothing about whether that response was a success: a 401 for a wrong App key is a reachable GitHub.", Presence: "always"},
		{Name: "github_last_call_at", Meaning: "When the `github_reachable` verdict was formed (Unix seconds).", Presence: "**absent** when no GitHub call has been classified since start-up", MayBeAbsent: true},
		{Name: "github_rate_observed", Meaning: "Whether any GitHub response has carried `X-RateLimit-*` headers.", Presence: "always"},
		{Name: "github_rate_remaining", Meaning: "The **lowest** remaining quota across quota buckets.", Presence: "always; meaningful only while `github_rate_observed` is true"},
		{Name: "github_rate_reset_at", Meaning: "Reset time of the bucket that holds that minimum (Unix seconds).", Presence: "always; same condition, 0 when unknown"},
		{Name: "pre_auth_limiter_entries", Meaning: "Entries in the pre-authentication per-IP rate limiter.", Presence: "always"},
		{Name: "reject_windows_open", Meaning: "Open `trigger_rejected` coalescing windows.", Presence: "always"},
		{Name: "metrics_drift", Meaning: "Disagreements between the metric catalog and what the code records: an undeclared name, a type clash, a label-set mismatch, or a recording under a metric declared as not emitted.", Presence: "**absent** when there is no disagreement", MayBeAbsent: true},
	}
}

// healthExample is a healthy moment: GitHub has been called at least once
// (so github_last_call_at is present) and the metric catalog agrees with the
// code (so metrics_drift is absent).
var healthExample = adminHealth{
	Status:             "ok",
	Version:            "0.1.0-phase0",
	UptimeSeconds:      3821,
	SQLiteOK:           true,
	SchemaVersion:      3,
	GitHubReachable:    true,
	GitHubLastCallAt:   1724164280,
	GitHubRateObserved: true,
	GitHubRateRemain:   4231,
	GitHubRateResetAt:  1724164320,
	PreAuthLimiterSize: 12,
	RejectWindowsOpen:  0,
}

// HealthExampleJSON returns an indented example of the detailed /healthz body
// at a healthy moment, produced by the same encoder the handler uses.
func HealthExampleJSON() ([]byte, error) {
	return json.MarshalIndent(healthExample, "", "  ")
}
