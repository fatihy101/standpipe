package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/metrics"
)

// ---------------------------------------------------------------------------
// parsePromText — `standpipe status` reads the LIVE gateway through /metrics.
//
// It is a parser of somebody else's output format, which is the shape that
// fails quietly: a label value containing a space or a comma splits a sample
// into the wrong bucket, and `status` then reports a quota or a coverage number
// that belongs to a different series. Nothing errors.
// ---------------------------------------------------------------------------

func TestParsePromTextAgainstTheRegistrysOwnOutput(t *testing.T) {
	reg := metrics.New()
	reg.SetGauge("gateway_github_rate_remaining", 4321, map[string]string{"bucket": "installation:5001"})
	reg.SetGauge("gateway_github_rate_remaining", 17, map[string]string{"bucket": "app"})
	reg.IncCounter("gateway_trigger_rejected_total", map[string]string{
		"caller": "release-bot", "reason": "not_in_allowlist",
	})
	reg.SetGauge("gateway_uptime_seconds", 12.5, nil)

	var sb strings.Builder
	if err := reg.Write(&sb); err != nil {
		t.Fatalf("registry.Write: %v", err)
	}
	got := parsePromText(sb.String())

	rates := got["gateway_github_rate_remaining"]
	if len(rates) != 2 {
		t.Fatalf("gateway_github_rate_remaining: %d sample(s), want 2: %+v", len(rates), rates)
	}
	byBucket := map[string]float64{}
	for _, s := range rates {
		byBucket[s.Labels["bucket"]] = s.Value
	}
	if byBucket["installation:5001"] != 4321 || byBucket["app"] != 17 {
		t.Errorf("samples landed in the wrong buckets: %+v", byBucket)
	}

	// Two labels on one series: the comma split is where a naive parser breaks.
	rej := got["gateway_trigger_rejected_total"]
	if len(rej) != 1 {
		t.Fatalf("gateway_trigger_rejected_total: %d sample(s), want 1", len(rej))
	}
	if rej[0].Labels["caller"] != "release-bot" || rej[0].Labels["reason"] != "not_in_allowlist" {
		t.Errorf("labels = %+v, want caller=release-bot reason=not_in_allowlist", rej[0].Labels)
	}

	if un := got["gateway_uptime_seconds"]; len(un) != 1 || un[0].Value != 12.5 {
		t.Errorf("unlabelled sample = %+v, want one sample of 12.5", un)
	}
	if _, ok := got["# HELP"]; ok {
		t.Error("a comment line was parsed as a metric")
	}
}

func TestParsePromTextSurvivesAwkwardLabelValues(t *testing.T) {
	// The registry escapes `"` and `\` in label values, and
	// a value may legitimately contain a space or a comma — a reject reason, a
	// repo name, an operator-chosen caller name.
	text := `
# HELP x help text with spaces
# TYPE x gauge
x{a="one, two",b="quote \" inside"} 3
y 4
z{} 5
malformed_no_value
also{bad="x"} notanumber
`
	got := parsePromText(text)
	xs := got["x"]
	if len(xs) != 1 {
		t.Fatalf("x: %d sample(s), want 1: %+v", len(xs), xs)
	}
	if xs[0].Labels["a"] != "one, two" {
		t.Errorf("label a = %q, want %q — the comma split ran inside a quoted value", xs[0].Labels["a"], "one, two")
	}
	if xs[0].Value != 3 {
		t.Errorf("x = %v, want 3", xs[0].Value)
	}
	if len(got["y"]) != 1 || got["y"][0].Value != 4 {
		t.Errorf("y = %+v, want one sample of 4", got["y"])
	}
	if len(got["z"]) != 1 || got["z"][0].Value != 5 {
		t.Errorf("z = %+v, want one sample of 5", got["z"])
	}
	// A line that cannot be read is DROPPED, never guessed at.
	if len(got["malformed_no_value"]) != 0 || len(got["also"]) != 0 {
		t.Errorf("an unparseable line produced a sample: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// parseTimestamp — operator input for `forget --before` and `export --since`.
//
// `forget` DELETES. A timestamp silently read as the wrong instant is the one
// input in this CLI whose misparse destroys data, so the accepted forms are
// pinned and the rejected ones with them.
// ---------------------------------------------------------------------------

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-08-01T12:30:00Z", time.Date(2026, 8, 1, 12, 30, 0, 0, time.UTC)},
		{"2026-08-01T12:30:00+02:00", time.Date(2026, 8, 1, 10, 30, 0, 0, time.UTC)},
		{"2026-08-01", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{"  2026-08-01  ", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		// A `date +%s` value: seconds.
		{"1756000000", time.Unix(1756000000, 0)},
		// What the event log stores: milliseconds. The 1e12 boundary is what
		// tells them apart, and reading millis as seconds would put the cutoff
		// in the year 57000 and purge EVERYTHING.
		{"1756000000000", time.UnixMilli(1756000000000)},
	}
	for _, tc := range cases {
		got, err := parseTimestamp(tc.in)
		if err != nil {
			t.Errorf("parseTimestamp(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseTimestamp(%q) = %s, want %s", tc.in, got.UTC(), tc.want.UTC())
		}
	}

	for _, bad := range []string{"", "   ", "yesterday", "2026-13-01", "2026-08-01 12:30", "1e9"} {
		if got, err := parseTimestamp(bad); err == nil {
			t.Errorf("parseTimestamp(%q) = %s, want an error — `forget --before` deletes on this value",
				bad, got)
		}
	}
}

// ---------------------------------------------------------------------------
// routing.json.
//
// The file's whole purpose is to be readable by a CALLER during an outage, with
// no gateway involved. A structurally broken one is discovered at the worst
// possible moment, which is why `standpipe inventory --routing` exits non-zero on
// it rather than printing a footnote.
// ---------------------------------------------------------------------------

func TestValidateRouting(t *testing.T) {
	valid := NewRoutingFile("ops@acme", "initial_observe_mode", time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC))
	rawOf := func(f RoutingFile) map[string]any {
		t.Helper()
		buf, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(buf, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		return m
	}

	if errs := ValidateRouting(valid, rawOf(valid)); len(errs) != 0 {
		t.Fatalf("the document NewRoutingFile produces does not validate: %v", errs)
	}
	// `"overrides": {}` is a STATEMENT that there are none. null would
	// make a caller's overrides[workflow] lookup a decision about a missing map.
	if valid.Overrides == nil {
		t.Error("NewRoutingFile left overrides nil; the file format requires an empty object")
	}

	cases := []struct {
		name     string
		mutate   func(*RoutingFile)
		dropRaw  string
		addRaw   map[string]any
		wantHint string
	}{
		{"wrong schema_version", func(f *RoutingFile) { f.SchemaVersion = 2 }, "", nil, "schema_version"},
		{"wrong default_backend", func(f *RoutingFile) { f.DefaultBackend = "act" }, "", nil, "default_backend"},
		{"missing updated_at", func(f *RoutingFile) { f.UpdatedAt = "" }, "", nil, "updated_at is required"},
		{"malformed updated_at", func(f *RoutingFile) { f.UpdatedAt = "21/08/2026" }, "", nil, "not RFC 3339"},
		{"missing updated_by", func(f *RoutingFile) { f.UpdatedBy = "" }, "", nil, "updated_by is required"},
		{"missing reason", func(f *RoutingFile) { f.Reason = "" }, "", nil, "reason is required"},
		{
			// A Phase 2 routing rule written into a Phase 0 file: it would be read
			// by nobody and act on nothing, which is a silent success.
			"non-empty overrides",
			func(f *RoutingFile) { f.Overrides = map[string]string{"release.yml": "act"} },
			"", nil, "overrides must be empty",
		},
		{"overrides key absent entirely", nil, "overrides", nil, "overrides is missing"},
		{"an unknown field", nil, "", map[string]any{"backend_weights": 1}, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := valid
			if tc.mutate != nil {
				tc.mutate(&f)
			}
			raw := rawOf(f)
			if tc.dropRaw != "" {
				delete(raw, tc.dropRaw)
			}
			for k, v := range tc.addRaw {
				raw[k] = v
			}
			errs := ValidateRouting(f, raw)
			if len(errs) == 0 {
				t.Fatalf("no error reported; want one mentioning %q", tc.wantHint)
			}
			joined := strings.Join(errs, " | ")
			if !strings.Contains(joined, tc.wantHint) {
				t.Errorf("errors = %v, want one mentioning %q", errs, tc.wantHint)
			}
		})
	}

	// EVERY problem, not the first: an operator fixing a file one error per run
	// gives up.
	broken := RoutingFile{SchemaVersion: 9, DefaultBackend: "act"}
	if errs := ValidateRouting(broken, map[string]any{}); len(errs) < 4 {
		t.Errorf("a document with four problems reported %d error(s): %v", len(errs), errs)
	}
}

// ---------------------------------------------------------------------------
// `standpipe onboard`'s YAML writer.
//
// Four properties, each a separate failure, and three of them destroy an
// operator's file rather than merely misbehaving.
// ---------------------------------------------------------------------------

const onboardBaseYAML = `# The gateway's configuration. Comments here explain WHY.
gateway:
  listen: ":8443"
  admin_listen: "127.0.0.1:9090"
  rate_limit:
    per_caller: {rate_per_second: 5, burst: 20}
    global: {rate_per_second: 5, burst: 100}
    pre_auth_ip: {rate_per_second: 2, burst: 10}
storage:
  sqlite_path: "/var/lib/standpipe/gateway.db"
github:
  app_id: 12345
  private_key_path: "/etc/standpipe/app.pem"
allowed_targets:
  # release.yml is pinned to main on purpose: a tag build would deploy.
  - repo: "acme/core"
    workflows: ["release.yml"]
    refs: ["refs/heads/main"]
callers:
  release-bot:
    token_hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
    token_id: "tok_a1"
    role: "caller"
    allow:
      - repo: "acme/core"
        workflow: "release.yml"
        ref: "refs/heads/main"
`

func TestUpsertAllowedTargetProperties(t *testing.T) {
	t.Run("comments survive the edit", func(t *testing.T) {
		out, changed, err := upsertAllowedTarget([]byte(onboardBaseYAML), "acme/core", []string{"test.yml"})
		if err != nil {
			t.Fatalf("upsertAllowedTarget: %v", err)
		}
		if !changed {
			t.Fatal("adding a new workflow reported no change")
		}
		for _, comment := range []string{
			"# The gateway's configuration. Comments here explain WHY.",
			"# release.yml is pinned to main on purpose: a tag build would deploy.",
		} {
			if !strings.Contains(string(out), comment) {
				t.Errorf("the edit deleted a comment: %q\n---\n%s", comment, out)
			}
		}
	})

	t.Run("re-running is idempotent", func(t *testing.T) {
		once, _, err := upsertAllowedTarget([]byte(onboardBaseYAML), "acme/core", []string{"test.yml"})
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		twice, changed, err := upsertAllowedTarget(once, "acme/core", []string{"test.yml"})
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if changed {
			t.Error("a second identical run reported a change; onboarding requires the same outcome")
		}
		if string(twice) != string(once) {
			t.Errorf("a second identical run rewrote the file:\n--- first\n%s\n--- second\n%s", once, twice)
		}
		// And it must not have created a SECOND entry for the repo, which
		// config.Validate rejects outright.
		cfg, err := config.Parse(twice)
		if err != nil {
			t.Fatalf("the twice-edited file does not validate: %v\n%s", err, twice)
		}
		seen := 0
		for _, tgt := range cfg.AllowedTargets {
			if tgt.Repo == "acme/core" {
				seen++
			}
		}
		if seen != 1 {
			t.Errorf("%d entries for acme/core; a duplicate repo fails config.Validate", seen)
		}
	})

	t.Run("existing workflows are kept and the order is not churned", func(t *testing.T) {
		out, _, err := upsertAllowedTarget([]byte(onboardBaseYAML), "acme/core", []string{"test.yml", "build.yml"})
		if err != nil {
			t.Fatalf("upsertAllowedTarget: %v", err)
		}
		cfg, err := config.Parse(out)
		if err != nil {
			t.Fatalf("config.Parse: %v\n%s", err, out)
		}
		var got []string
		for _, tgt := range cfg.AllowedTargets {
			if tgt.Repo == "acme/core" {
				got = tgt.Workflows
			}
		}
		want := []string{"release.yml", "build.yml", "test.yml"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("workflows = %v, want %v — existing entries keep their place and additions are "+
				"appended, so a diff shows the one line that changed", got, want)
		}
	})

	t.Run("a new repo is appended and the result still validates", func(t *testing.T) {
		out, changed, err := upsertAllowedTarget([]byte(onboardBaseYAML), "acme/tools", []string{"ci.yml"})
		if err != nil {
			t.Fatalf("upsertAllowedTarget: %v", err)
		}
		if !changed {
			t.Fatal("adding a new repo reported no change")
		}
		cfg, err := config.Parse(out)
		if err != nil {
			t.Fatalf("config.Parse: %v\n%s", err, out)
		}
		found := false
		for _, tgt := range cfg.AllowedTargets {
			if tgt.Repo == "acme/tools" && len(tgt.Workflows) == 1 && tgt.Workflows[0] == "ci.yml" {
				found = true
			}
		}
		if !found {
			t.Errorf("acme/tools was not added:\n%s", out)
		}
	})

	t.Run("a malformed file is refused, not rewritten", func(t *testing.T) {
		for _, bad := range []string{
			"",
			"allowed_targets: \"not a list\"\n",
			"- a\n- b\n",
		} {
			if _, _, err := upsertAllowedTarget([]byte(bad), "acme/core", []string{"x.yml"}); err == nil {
				t.Errorf("upsertAllowedTarget accepted %q", bad)
			}
		}
	})
}

// TestWriteConfigAtomicallyRefusesToClobber is the compare-before-rename: the
// gateway only reads standpipe.yaml, so there is no lock, and renaming over
// somebody else's edit would discard it with no trace.
func TestWriteConfigAtomicallyRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "standpipe.yaml")
	original := []byte(onboardBaseYAML)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	next, _, err := upsertAllowedTarget(original, "acme/core", []string{"test.yml"})
	if err != nil {
		t.Fatalf("upsertAllowedTarget: %v", err)
	}

	// Somebody else edits the file in between.
	meddled := append([]byte("# somebody else was here\n"), original...)
	if err := os.WriteFile(path, meddled, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := writeConfigAtomically(path, original, next); err == nil {
		t.Fatal("the concurrent edit was overwritten silently")
	}
	if got, _ := os.ReadFile(path); string(got) != string(meddled) {
		t.Error("the other edit was lost even though the write was refused")
	}

	// And the happy path does replace it.
	if err := writeConfigAtomically(path, meddled, next); err != nil {
		t.Fatalf("writeConfigAtomically: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(next) {
		t.Error("the file was not replaced")
	}

	// A file that does not validate is NEVER written. `onboard` sends SIGHUP
	// afterwards, and a gateway running on a configuration nobody can reload is
	// the worst outcome of this command.
	if err := writeConfigAtomically(path, next, []byte("gateway:\n  listen: 42\n")); err == nil {
		t.Fatal("an invalid configuration was written")
	}
	if got, _ := os.ReadFile(path); string(got) != string(next) {
		t.Error("the file changed even though validation failed")
	}
}
