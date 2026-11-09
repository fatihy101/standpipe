package store

import (
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// HashScopes() versus the scope each writer actually hashes.
//
// content_sha256 is the only thing standing between the trigger log and a
// forgeable audit record, and what it is computed over is a DATA INVARIANT: a
// field that leaves the scope stays in the row, in `standpipe trace` and in an
// export, and becomes editable by anyone with write access to the database
// while `standpipe verify` goes on answering ok.
//
// HashScopes() is the closed list docs/reference/hash-scopes.md is generated
// from. This file compares it with the writer in BOTH directions:
//
//   - a key the writer hashes that the list does not declare means the log is
//     hashed over a field no reader knows is protected, and a later "cleanup"
//     of that field silently invalidates every stored hash;
//   - a field the list declares that the writer does not hash means the
//     reference promises integrity that does not exist: the forgeable case,
//     invisible to every runtime surface.
//
// The code side is derived by CALLING the scope builder and flattening it, the
// same derivation assertScopeIsClosed in content_hash_test.go uses. That file's
// sensitivity tables prove each key actually moves the hash; this one proves
// the published list names exactly those keys.
// ---------------------------------------------------------------------------

// scopeFieldRE is the shape a closed-list entry may take: a lower-snake
// identifier with at most one level of nesting.
var scopeFieldRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$`)

// scopeBuilders maps each event type to its writer's scope builder. populated
// asks for a fully valued event; the key SET must not depend on it.
func scopeBuilders() map[string]func(populated bool) map[string]any {
	return map[string]func(bool) map[string]any{
		string(EventTrigger): func(populated bool) map[string]any {
			if !populated {
				return triggerHashScope(TriggerEvent{})
			}
			return triggerHashScope(sampleTrigger("evt_ref_scope"))
		},
		string(EventTriggerOutcome): func(populated bool) map[string]any {
			if !populated {
				return triggerOutcomeHashScope(TriggerOutcomeEvent{})
			}
			return triggerOutcomeHashScope(TriggerOutcomeEvent{
				EventID: "evt_ref_out", TS: testBase(), TriggerEventID: "evt_ref_scope",
				OriginForge: "github", Epoch: 0,
				OutcomeStatus: OutcomeForwarded, OutcomeReason: "",
				OutcomeHTTPStatus: 200, UpstreamDuration: 342 * time.Millisecond,
			})
		},
		string(EventTriggerRejected): func(populated bool) map[string]any {
			if !populated {
				return triggerRejectedHashScope(TriggerRejectedEvent{})
			}
			ev := sampleRejected("evt_ref_rej")
			ev.RejectReason = RejectRateLimited
			ev.SuppressedCount = 7
			ev.SuppressedSince = testBase().Add(-time.Minute)
			return triggerRejectedHashScope(ev)
		},
		string(EventRunLink): func(populated bool) map[string]any {
			if !populated {
				return runLinkHashScope(RunLinkEvent{})
			}
			return runLinkHashScope(RunLinkEvent{
				EventID: "evt_ref_link", TS: testBase(), TriggerEventID: "evt_ref_scope",
				RunID: runID(4242), Resolution: ResolutionExact,
				ResolutionSource: SourceRunName,
				OriginForge:      "github", Epoch: 0,
			})
		},
		string(EventRunObserved): func(populated bool) map[string]any {
			if !populated {
				return runObservedHashScope(RunObservedEvent{})
			}
			return runObservedHashScope(RunObservedEvent{
				EventID: "evt_ref_obs", TS: testBase(), TriggerEventID: "evt_ref_scope",
				RunID: 4242, Conclusion: "success",
				OriginForge: "github", Epoch: 0,
			})
		},
	}
}

// TestEveryEventTypeHasADeclaredHashScope ties the list of scopes to the
// list of builders, so a sixth event type cannot ship with a writer and no
// published scope, or the reverse.
func TestEveryEventTypeHasADeclaredHashScope(t *testing.T) {
	builders := scopeBuilders()
	declared := map[string]bool{}
	for _, s := range HashScopes() {
		if declared[s.EventType] {
			t.Errorf("HashScopes() declares %q twice", s.EventType)
		}
		declared[s.EventType] = true
		if _, ok := builders[s.EventType]; !ok {
			t.Errorf("HashScopes() declares %q and this test has no scope builder for it", s.EventType)
		}
	}
	for et := range builders {
		if !declared[et] {
			t.Errorf("event type %q has a hash-scope builder and no entry in HashScopes()", et)
		}
	}
}

// TestHashScopesMatchTheWriters is the pin.
func TestHashScopesMatchTheWriters(t *testing.T) {
	builders := scopeBuilders()
	redactable := RedactableFields()

	for _, spec := range HashScopes() {
		t.Run(spec.EventType, func(t *testing.T) {
			build, ok := builders[spec.EventType]
			if !ok {
				t.Skip("reported by TestEveryEventTypeHasADeclaredHashScope")
			}
			if len(spec.Fields) == 0 {
				t.Fatalf("%s declares an empty hash scope", spec.EventType)
			}

			actual := map[string]bool{}
			flattenScope("", build(true), actual)
			if len(actual) == 0 {
				t.Fatalf("%s's writer produced an EMPTY hash scope; every comparison below would pass "+
					"vacuously in the direction that matters", spec.EventType)
			}

			// A closed list is a list of NAMES, so it can only describe a key
			// set that does not depend on the event's values.
			zero := map[string]bool{}
			flattenScope("", build(false), zero)
			for k := range actual {
				if !zero[k] {
					t.Errorf("%s's hash scope emits the key %q for a populated event and not for a zero one.\n"+
						"A conditional key cannot be described by a closed list: the reference would be right\n"+
						"for some rows and wrong for others. Emit the key unconditionally with a null value.",
						spec.EventType, k)
				}
			}
			for k := range zero {
				if !actual[k] {
					t.Errorf("%s's hash scope emits the key %q for a zero event and not for a populated one; "+
						"the key set must be structural", spec.EventType, k)
				}
			}

			declared := map[string]bool{}
			for _, f := range spec.Fields {
				if !scopeFieldRE.MatchString(f) {
					t.Errorf("%s declares %q, which is not a dotted field name", spec.EventType, f)
				}
				if declared[f] {
					t.Errorf("%s declares %q twice", spec.EventType, f)
				}
				declared[f] = true
				if !actual[f] {
					t.Errorf("HashScopes() declares %q in %s's scope and the writer does not hash it.\n"+
						"This is the forgeable direction: the column is written, exported and traced, and it is\n"+
						"not covered by content_sha256, so it can be edited in place and `verify` still answers\n"+
						"ok. Repair the writer, or remove the field from the list deliberately.\n"+
						"The writer hashes: %v", f, spec.EventType, sortedKeys(actual))
				}
			}
			for _, k := range sortedKeys(actual) {
				if !declared[k] {
					t.Errorf("%s's writer hashes %q and HashScopes() does not declare it.\n"+
						"The hash is now taken over a field no reader of the reference knows is protected.\n"+
						"If the field entered the scope on purpose, add it to the list and run\n"+
						"`go generate ./...`; if by accident, remove it from the writer.", spec.EventType, k)
				}
			}

			// An exclusion is a promise that the field is NOT hashed.
			for _, ex := range spec.Excluded {
				if strings.TrimSpace(ex.Reason) == "" {
					t.Errorf("%s excludes %q without a reason", spec.EventType, ex.Field)
				}
				if actual[ex.Field] || declared[ex.Field] {
					t.Errorf("%s lists %q as excluded from the hash scope, and it is inside it", spec.EventType, ex.Field)
				}
			}
			// The hash field itself is never inside its own scope.
			if actual["content_sha256"] {
				t.Errorf("%s's hash scope contains content_sha256, the field it computes", spec.EventType)
			}

			if !spec.Redactable {
				return
			}
			// `forget` covers this row type. A redactable field inside the
			// scope means the row stops verifying the moment it is redacted.
			for _, field := range redactable {
				if actual[field] {
					t.Errorf("%q is redactable and %s's hash scope CONTAINS it. `standpipe forget` NULLs that\n"+
						"column, so every row it touches would stop verifying.", field, spec.EventType)
				}
				excluded := false
				for _, ex := range spec.Excluded {
					excluded = excluded || ex.Field == field
				}
				if !excluded {
					t.Errorf("%s is redactable and its reference does not list %q as excluded", spec.EventType, field)
				}
			}
		})
	}
}

// TestRedactableFieldsAreWhatForgetClears binds the published redactable
// fields and row types to the SQL `standpipe forget` runs.
func TestRedactableFieldsAreWhatForgetClears(t *testing.T) {
	for _, f := range RedactableFields() {
		col := strings.ReplaceAll(f, ".", "_")
		if !strings.Contains(forgetPredicate, col+" IS NOT NULL") {
			t.Errorf("RedactableFields() lists %q, and Forget's predicate does not look at column %s:\n%s",
				f, col, forgetPredicate)
		}
	}

	var want []string
	for _, s := range HashScopes() {
		if s.Redactable {
			want = append(want, "'"+s.EventType+"'")
		}
	}
	sort.Strings(want)
	got := strings.Split(strings.Trim(redactableEventTypes, "()"), ",")
	for i := range got {
		got[i] = strings.TrimSpace(got[i])
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Forget redacts event types %v and HashScopes() marks %v as redactable", got, want)
	}
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
