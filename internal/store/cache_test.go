package store

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestWorkflowCacheInputsDefNullVsEmptyObject is the distinction unsupported-input
// validation hangs on.
//
//	""   -> SQL NULL -> the workflow file could not be read. Validation is
//	                    SKIPPED and the event is marked skipped_cold_cache.
//	"{}" -> '{}'     -> the file WAS read and declares no inputs, so any input
//	                    key the caller sends is a 422 unsupported_inputs.
//
// Collapsing the two is silent degradation: a workflow that
// could not be read would start rejecting every input as unsupported, or a
// workflow that declares none would start accepting anything.
func TestWorkflowCacheInputsDefNullVsEmptyObject(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	entries := []WorkflowCacheEntry{
		{Repo: "acme/core", Workflow: "unknown.yml", Ref: "default", WorkflowID: 1,
			Triggers: []string{"workflow_dispatch"}, InputsDef: "", DefaultBranch: "main", FetchedAt: testBase()},
		{Repo: "acme/core", Workflow: "noinputs.yml", Ref: "default", WorkflowID: 2,
			Triggers: []string{"workflow_dispatch"}, InputsDef: "{}", DefaultBranch: "main", FetchedAt: testBase()},
		{Repo: "acme/core", Workflow: "release.yml", Ref: "default", WorkflowID: 3,
			Triggers:      []string{"workflow_dispatch", "push"},
			InputsDef:     `{"tag":{"required":true},"standby_trigger_id":{"required":false}}`,
			DefaultBranch: "main", FetchedAt: testBase()},
	}
	if err := env.UpsertWorkflowCache(c, entries); err != nil {
		t.Fatalf("UpsertWorkflowCache: %v", err)
	}

	// Storage layer: one is SQL NULL, the other is the two-character string.
	var raw sql.NullString
	env.rawScan(t, []any{&raw}, `SELECT inputs_def FROM workflow_cache WHERE workflow = ?`, "unknown.yml")
	if raw.Valid {
		t.Errorf(`inputs_def for the unreadable workflow = %q, want SQL NULL`, raw.String)
	}
	env.rawScan(t, []any{&raw}, `SELECT inputs_def FROM workflow_cache WHERE workflow = ?`, "noinputs.yml")
	if !raw.Valid || raw.String != "{}" {
		t.Errorf(`inputs_def for the empty-inputs workflow = %v/%q, want '{}'`, raw.Valid, raw.String)
	}

	// Round trip through the API.
	unknown, err := env.WorkflowDefinition(c, "acme/core", "unknown.yml", "default")
	if err != nil {
		t.Fatalf("WorkflowDefinition: %v", err)
	}
	if unknown.InputsDef != "" {
		t.Errorf("InputsDef = %q, want the empty string (definition unknown)", unknown.InputsDef)
	}
	noinputs, err := env.WorkflowDefinition(c, "acme/core", "noinputs.yml", "default")
	if err != nil {
		t.Fatalf("WorkflowDefinition: %v", err)
	}
	if noinputs.InputsDef != "{}" {
		t.Errorf(`InputsDef = %q, want "{}"`, noinputs.InputsDef)
	}

	// The distinction survives an upsert in either direction.
	entries[0].InputsDef = "{}"
	entries[1].InputsDef = ""
	if err := env.UpsertWorkflowCache(c, entries[:2]); err != nil {
		t.Fatalf("UpsertWorkflowCache: %v", err)
	}
	unknown, _ = env.WorkflowDefinition(c, "acme/core", "unknown.yml", "default")
	noinputs, _ = env.WorkflowDefinition(c, "acme/core", "noinputs.yml", "default")
	if unknown.InputsDef != "{}" || noinputs.InputsDef != "" {
		t.Errorf("after swapping, got %q and %q; want \"{}\" and \"\"", unknown.InputsDef, noinputs.InputsDef)
	}
}

// TestWorkflowCacheColdIsAnError: a never-fetched workflow is ErrNotFound, and
// the caller has to surface that as cold. Silence here becomes silent
// degradation upstream.
func TestWorkflowCacheColdIsAnError(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	if _, err := env.WorkflowDefinition(c, "acme/core", "never-seen.yml", "default"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("WorkflowDefinition on a cold cache = %v, want ErrNotFound", err)
	}
}

// TestWorkflowCacheStaleEntryIsStillReturned: the workflow cache's stale-while-revalidate
// rule says only a never-fetched workflow is cold. An old entry still answers.
func TestWorkflowCacheStaleEntryIsStillReturned(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	if err := env.UpsertWorkflowCache(c, []WorkflowCacheEntry{{
		Repo: "acme/core", Workflow: "old.yml", WorkflowID: 1,
		Triggers: []string{"workflow_dispatch"}, InputsDef: "{}",
		FetchedAt: testBase().Add(-365 * 24 * time.Hour),
	}}); err != nil {
		t.Fatalf("UpsertWorkflowCache: %v", err)
	}
	env.clock.Set(testBase().Add(30 * 24 * time.Hour))
	got, err := env.WorkflowDefinition(c, "acme/core", "old.yml", "")
	if err != nil {
		t.Fatalf("WorkflowDefinition on a stale entry = %v, want the entry", err)
	}
	if !got.FetchedAt.Equal(testBase().Add(-365 * 24 * time.Hour)) {
		t.Errorf("FetchedAt = %v, want the original fetch time so the caller can judge staleness", got.FetchedAt)
	}
	// An empty ref resolves to "default".
	if got.Ref != "default" {
		t.Errorf("Ref = %q, want default", got.Ref)
	}
}

// TestWorkflowCacheRefIsPartOfTheKey: different branches can declare different
// inputs, so (repo, workflow, ref) is the key.
func TestWorkflowCacheRefIsPartOfTheKey(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	if err := env.UpsertWorkflowCache(c, []WorkflowCacheEntry{
		{Repo: "acme/core", Workflow: "release.yml", Ref: "default", WorkflowID: 1,
			Triggers: []string{"workflow_dispatch"}, InputsDef: `{"tag":{}}`},
		{Repo: "acme/core", Workflow: "release.yml", Ref: "refs/heads/next", WorkflowID: 1,
			Triggers: []string{"workflow_dispatch"}, InputsDef: `{"tag":{},"canary":{}}`},
	}); err != nil {
		t.Fatalf("UpsertWorkflowCache: %v", err)
	}
	def, err := env.WorkflowDefinition(c, "acme/core", "release.yml", "default")
	if err != nil {
		t.Fatalf("WorkflowDefinition: %v", err)
	}
	if def.InputsDef != `{"tag":{}}` {
		t.Errorf("default ref definition = %q", def.InputsDef)
	}
	next, err := env.WorkflowDefinition(c, "acme/core", "release.yml", "refs/heads/next")
	if err != nil {
		t.Fatalf("WorkflowDefinition: %v", err)
	}
	if next.InputsDef != `{"tag":{},"canary":{}}` {
		t.Errorf("next-branch definition = %q", next.InputsDef)
	}

	all, err := env.WorkflowCacheAll(c)
	if err != nil {
		t.Fatalf("WorkflowCacheAll: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("WorkflowCacheAll returned %d entries, want 2", len(all))
	}
}

// TestCorrelationReady is the correlation_ready adoption signal: it must be false whenever the
// definition is unknown or unparseable, because "we do not know" must never
// look like "yes".
func TestCorrelationReady(t *testing.T) {
	cases := []struct {
		name      string
		inputsDef string
		want      bool
	}{
		{"declared", `{"tag":{},"standby_trigger_id":{"required":false}}`, true},
		{"declared alone", `{"standby_trigger_id":{}}`, true},
		{"not declared", `{"tag":{}}`, false},
		{"no inputs at all", `{}`, false},
		{"definition unknown (cold)", ``, false},
		{"unparseable definition fails closed", `{not json`, false},
		{"json array instead of object fails closed", `[]`, false},
		{"whitespace only", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := WorkflowCacheEntry{InputsDef: tc.inputsDef}
			if got := e.CorrelationReady(); got != tc.want {
				t.Fatalf("CorrelationReady() = %v, want %v", got, tc.want)
			}
		})
	}
	if ReservedInputKey() != "standby_trigger_id" {
		t.Errorf("ReservedInputKey() = %q, want standby_trigger_id", ReservedInputKey())
	}
}

// TestUpsertWorkflowCacheValidation.
func TestUpsertWorkflowCacheValidation(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)
	if err := env.UpsertWorkflowCache(c, nil); err != nil {
		t.Errorf("UpsertWorkflowCache(nil) = %v, want nil", err)
	}
	for _, e := range []WorkflowCacheEntry{
		{Workflow: "release.yml"},
		{Repo: "acme/core"},
	} {
		if err := env.UpsertWorkflowCache(c, []WorkflowCacheEntry{e}); err == nil {
			t.Errorf("UpsertWorkflowCache accepted %+v", e)
		}
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM workflow_cache`); n != 0 {
		t.Errorf("%d rows written by rejected entries, want 0", n)
	}
}

// TestInputsHashIsKeyedAndKeyIdentified: inputs_hash must not be
// a plain digest, or the log becomes a verification oracle for guessed secrets;
// and the key it names must be identified by a RANDOM id, not a counter.
//
// The counter is not a cosmetic difference. The HMAC key lives inside the
// backup snapshot, so restoring a snapshot taken before a key was minted makes
// the gateway mint a fresh one. Under a counter that key is `v1` again, and
// every surviving value from the PREVIOUS v1 key then verifies as "hash
// mismatch" — the log reports tampering where the honest answer is "the key
// that produced this value is not in this database". A silent mislabel of
// exactly the kind that must never pass silently.
func TestInputsHashIsKeyedAndKeyIdentified(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	canon, err := CanonicalJSON([]byte(`{"tag":"v1"}`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	h1, err := env.InputsHash(c, canon)
	if err != nil {
		t.Fatalf("InputsHash: %v", err)
	}
	id1 := assertInputsHashForm(t, h1)
	if h1 == SHA256Hex(canon) {
		t.Fatalf("InputsHash equals a plain SHA-256; inputs_hash must be a keyed hash")
	}
	if err := env.VerifyInputsHash(c, canon, h1); err != nil {
		t.Fatalf("VerifyInputsHash on a freshly produced value: %v", err)
	}

	// Same input, same key -> same value; and the key survives a restart.
	env.reopen(t)
	h2, err := env.InputsHash(c, canon)
	if err != nil {
		t.Fatalf("InputsHash after reopen: %v", err)
	}
	if h2 != h1 {
		t.Fatalf("inputs_hash changed across a restart: %s -> %s; the HMAC key is not persistent", h1, h2)
	}

	// Rotation mints a NEW random id and changes the value; the scheme keeps older
	// values verifiable, because the value names the key that produced it.
	id2, err := env.RotateHMACKeyID(c)
	if err != nil {
		t.Fatalf("RotateHMACKeyID: %v", err)
	}
	if id2 == id1 {
		t.Errorf("RotateHMACKeyID returned the previous key id %s", id2)
	}
	if len(id2) != 16 {
		t.Errorf("rotated key_id = %q, want 16 hex characters", id2)
	}
	h3, err := env.InputsHash(c, canon)
	if err != nil {
		t.Fatalf("InputsHash after rotation: %v", err)
	}
	if h3 == h2 {
		t.Errorf("inputs_hash unchanged after a key rotation")
	}
	if got := assertInputsHashForm(t, h3); got != id2 {
		t.Errorf("inputs_hash after rotation names key %s, want the new key %s", got, id2)
	}
	// The old value still verifies under its own key id — the reason the id is
	// carried in the value at all.
	if err := env.VerifyInputsHash(c, canon, h1); err != nil {
		t.Errorf("the pre-rotation value no longer verifies: %v", err)
	}

	// Exactly one key is active.
	if n := env.countRows(t, `SELECT COUNT(*) FROM hmac_keys WHERE active = 1`); n != 1 {
		t.Errorf("%d active HMAC keys, want 1", n)
	}
	if n := env.countRows(t, `SELECT COUNT(*) FROM hmac_keys`); n != 2 {
		t.Errorf("%d HMAC keys, want 2 (the old one stays for verification)", n)
	}
}

// assertInputsHashForm checks the "hmac-sha256:<key_id>:<hex>" wire form and
// returns the key id. The key id must be a 16-character random hex string, and
// specifically NOT a "v1"-style counter.
func assertInputsHashForm(t *testing.T, value string) string {
	t.Helper()
	alg, rest, ok := strings.Cut(value, ":")
	if !ok || alg != "hmac-sha256" {
		t.Fatalf("inputs_hash = %q, want the hmac-sha256:<key_id>:<hex> form", value)
	}
	keyID, digest, ok := strings.Cut(rest, ":")
	if !ok {
		t.Fatalf("inputs_hash = %q carries no key_id", value)
	}
	if _, err := hex.DecodeString(keyID); err != nil || len(keyID) != 16 {
		t.Fatalf("key_id = %q, want 16 random hex characters (not a counter such as v1)", keyID)
	}
	if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
		t.Fatalf("digest = %q, want 64 hex characters of HMAC-SHA256", digest)
	}
	return keyID
}

// TestVerifyInputsHashUnknownKeyIsNotAMismatch is the reason the key id is random.
//
// A value produced by a key this database does not hold — the state a restore
// from a pre-key snapshot leaves behind — must be reported as an
// UNKNOWN KEY, not as a hash mismatch. A mismatch says "this record was
// tampered with"; saying that about an intact record is a false accusation
// produced by the store itself, and this class of confidently-wrong answer is
// the one the store must never give.
func TestVerifyInputsHashUnknownKeyIsNotAMismatch(t *testing.T) {
	env := newTestEnv(t)
	c := ctx(t)

	canon, err := CanonicalJSON([]byte(`{"tag":"v1"}`))
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	value, err := env.InputsHash(c, canon)
	if err != nil {
		t.Fatalf("InputsHash: %v", err)
	}
	_, rest, _ := strings.Cut(value, ":")
	_, digest, _ := strings.Cut(rest, ":")

	// Same digest, a key id no row holds: the snapshot-restore case.
	foreign := "hmac-sha256:00112233445566ff:" + digest
	err = env.VerifyInputsHash(c, canon, foreign)
	if !errors.Is(err, ErrUnknownHMACKey) {
		t.Fatalf("VerifyInputsHash with an unheld key_id = %v, want ErrUnknownHMACKey", err)
	}

	// A wrong digest under a KNOWN key is a genuine mismatch and must NOT be
	// reported as an unknown key — the two answers are not interchangeable.
	keyID := assertInputsHashForm(t, value)
	tampered := "hmac-sha256:" + keyID + ":" + strings.Repeat("0", 64)
	err = env.VerifyInputsHash(c, canon, tampered)
	if err == nil {
		t.Fatalf("VerifyInputsHash accepted a tampered digest under a known key")
	}
	if errors.Is(err, ErrUnknownHMACKey) {
		t.Errorf("a tampered digest under a KNOWN key reported ErrUnknownHMACKey: %v", err)
	}

	// Malformed values are rejected, not silently treated as unknown keys.
	for _, bad := range []string{"", "deadbeef", "sha256:deadbeef", "hmac-sha256:onlyonepart"} {
		if err := env.VerifyInputsHash(c, canon, bad); err == nil {
			t.Errorf("VerifyInputsHash accepted %q", bad)
		}
	}
}
