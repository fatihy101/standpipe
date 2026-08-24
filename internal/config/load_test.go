package config

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The loader's three-way rule: known / known-future / unknown.
//
// The known-future half is the only one of the three whose failure mode is a
// gateway that REFUSES TO START. An unknown key is meant to be an error, and a
// known key is meant to load; but a key that belongs to a phase the roadmap has
// already designed, and that this list has not been told about, turns "write the
// configuration the next phase's documentation tells you to write" into "the
// Phase 0 gateway does not come up". The `drill` block was exactly that case,
// and the precedent — `backends[].kind: act`, added to the list a phase early — is the same
// shape caught in time.
// ---------------------------------------------------------------------------

// minimalConfig is the smallest file Validate accepts. Parse runs Validate, so
// a known-future test needs one: otherwise every case would fail for reasons
// that have nothing to do with the root being tested.
const minimalConfig = `
gateway:
  listen: ":8443"
  admin_listen: "127.0.0.1:9090"
storage:
  sqlite_path: "/var/lib/standpipe/gateway.db"
github:
  app_id: 12345
  private_key_path: "/etc/standpipe/app.pem"
allowed_targets:
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

// drillBlock is the Phase 1 drill configuration block, copied whole rather than reduced to
// its required key: the assertion worth having is that the block an operator is
// told to write loads, not that some subset of it does.
const drillBlock = `
drill:
  timezone: "Europe/Istanbul"
  max_candidates_per_day: 8
  lookback_days: 3
  retention_days: 90
`

// TestDrillBlockLoadsWithAWarning: `drill.timezone` is required with no
// default in Phase 1, so the moment a drill-enabled installation writes its
// configuration, a Phase 0 gateway reading
// the SAME file must still start.
func TestDrillBlockLoadsWithAWarning(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig + drillBlock))
	if err != nil {
		t.Fatalf("a config carrying the Phase 1 drill block failed to load: %v\n"+
			"the known-future rule is what keeps Phase 0 and Phase 1 on one file.", err)
	}

	warn := warningFor(t, cfg.Warnings, "drill")
	for _, want := range []string{"known-future", "accepted and ignored", "Phase 1"} {
		if !strings.Contains(warn, want) {
			t.Errorf("drill warning = %q, want it to mention %q", warn, want)
		}
	}
	// The warning names the key that decides the day boundary. An operator
	// reading "drill: ignored" needs to know WHICH phase is going to read it.
	if !strings.Contains(warn, "drill.timezone") {
		t.Errorf("drill warning = %q, want it to name drill.timezone", warn)
	}
	if n := len(cfg.Warnings); n != 1 {
		t.Errorf("got %d warnings, want exactly 1: %v", n, cfg.Warnings)
	}
}

// TestDrillWithoutTimezoneStillStarts: Phase 1 makes `drill.timezone`
// required with NO default, and that requirement belongs to Phase 1's parser.
// A Phase 0 gateway neither enforces it nor supplies the `UTC` that would silently draw the day
// boundary and with it the 14/21 window.
func TestDrillWithoutTimezoneStillStarts(t *testing.T) {
	cfg, err := Parse([]byte(minimalConfig + `
drill:
  lookback_days: 3
`))
	if err != nil {
		t.Fatalf("a drill block with no timezone stopped a Phase 0 gateway: %v\n"+
			"the requirement is Phase 1's parser's, not this one's", err)
	}
	warningFor(t, cfg.Warnings, "drill")

	// Phase 0 supplies no default because it reads no value. Stated as an
	// equality rather than as a missing field: the block may add nothing to the
	// configuration at all except its warning. If a Drill field is ever added
	// here, this is the assertion that has to be argued with first.
	bare, err := Parse([]byte(minimalConfig))
	if err != nil {
		t.Fatalf("the minimal config does not load: %v", err)
	}
	cfg.Warnings, bare.Warnings = nil, nil
	if !reflect.DeepEqual(cfg, bare) {
		t.Errorf("a drill block changed the loaded configuration.\nwith drill: %+v\nwithout:    %+v", cfg, bare)
	}
}

// TestFutureRootsAreSkippedWhole guards the mechanism the entry rides on: the
// block is not walked key by key. The drill block's key set is Phase 1's to
// change, and a Phase 0 loader that validated it would be a second copy of a
// schema it does not own.
func TestFutureRootsAreSkippedWhole(t *testing.T) {
	// A key no phase has ever designed, under a known-future root.
	cfg, err := Parse([]byte(minimalConfig + `
drill:
  timezone: "UTC"
  a_key_no_document_has_ever_named: 1
`))
	if err != nil {
		t.Fatalf("a sub-key under a known-future root produced an error: %v", err)
	}
	if n := len(cfg.Warnings); n != 1 {
		t.Errorf("got %d warnings, want exactly 1 (the root, not one per sub-key): %v", n, cfg.Warnings)
	}
}

// TestEveryFutureRootLoads runs the whole list, so an entry cannot be added to
// the map in a form the walker never reaches.
func TestEveryFutureRootLoads(t *testing.T) {
	for root, label := range futureRoots {
		t.Run(root, func(t *testing.T) {
			cfg, err := Parse([]byte(minimalConfig + "\n" + root + ":\n  some_key: 1\n"))
			if err != nil {
				t.Fatalf("known-future root %q failed to load: %v", root, err)
			}
			if got := warningFor(t, cfg.Warnings, root); !strings.Contains(got, label) {
				t.Errorf("warning for %q = %q, want it to carry %q", root, got, label)
			}
			// Every entry says which phase will read the block. Without it the
			// warning tells an operator to keep a key but not who keeps it.
			if !strings.Contains(label, "Phase ") {
				t.Errorf("futureRoots[%q] = %q, want it to name a phase", root, label)
			}
		})
	}
}

// TestUnknownRootIsStillAnError is the other side. Widening the known-future
// list is only safe while a genuinely unknown key is still refused — including
// a near-miss spelling of an entry that WAS added.
func TestUnknownRootIsStillAnError(t *testing.T) {
	for _, root := range []string{"drills", "dril", "drill_day", "backend_weights"} {
		if _, err := Parse([]byte(minimalConfig + "\n" + root + ":\n  x: 1\n")); err == nil {
			t.Errorf("root %q loaded; a completely unknown field is a startup error", root)
		} else if !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("root %q: err = %v, want an `unknown field(s)` report", root, err)
		}
	}
}

func warningFor(t *testing.T, warnings []string, root string) string {
	t.Helper()
	var hits []string
	for _, w := range warnings {
		if strings.HasPrefix(w, root+":") {
			hits = append(hits, w)
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 1:
		return hits[0]
	case 0:
		t.Fatalf("no warning for %q; warnings = %v", root, warnings)
	}
	t.Fatalf("%d warnings for %q, want 1: %v", len(hits), root, hits)
	return ""
}
