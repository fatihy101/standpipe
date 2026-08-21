package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fatihy101/standpipe/internal/forge"
	"github.com/fatihy101/standpipe/internal/store"
	"github.com/fatihy101/standpipe/internal/testutil"
)

// ---------------------------------------------------------------------------
// The second zero-value hazard, and the invariants that fail silently.
//
// Everything here compiles, passes vet, and writes a legal enum value when it
// is wrong. That is the whole reason these tests exist: no failure in this file
// would produce an error, a log line or a non-2xx anywhere in the running
// system. The only symptom is a coverage report that slowly stops saying
// `covered`.
// ---------------------------------------------------------------------------

// stubDB is a StoreDB that answers from memory.
type stubDB struct {
	unresolved []store.UnresolvedTrigger
	unresolErr error

	entry    store.WorkflowCacheEntry
	entryErr error

	upserted []store.WorkflowCacheEntry
	links    []store.RunLinkEvent
	linkErr  error
	observed []store.RunObservedEvent
}

func (s *stubDB) WorkflowDefinition(context.Context, string, string, string) (store.WorkflowCacheEntry, error) {
	return s.entry, s.entryErr
}

func (s *stubDB) UpsertWorkflowCache(_ context.Context, e []store.WorkflowCacheEntry) error {
	s.upserted = append(s.upserted, e...)
	return nil
}

func (s *stubDB) UpsertGitHubRuns(context.Context, []store.GitHubRun) ([]store.ConclusionChange, error) {
	return nil, nil
}

func (s *stubDB) UnresolvedTriggers(context.Context, time.Duration, int) ([]store.UnresolvedTrigger, error) {
	return s.unresolved, s.unresolErr
}

func (s *stubDB) AppendRunLink(_ context.Context, ev store.RunLinkEvent) (string, error) {
	if s.linkErr != nil {
		return "", s.linkErr
	}
	s.links = append(s.links, ev)
	return "evt_link", nil
}

func (s *stubDB) AppendRunObserved(_ context.Context, ev store.RunObservedEvent) (string, error) {
	s.observed = append(s.observed, ev)
	return "evt_obs", nil
}

// TestPendingCorrelationsCarriesCorrelationThrough is the second zero-value hazard exactly.
//
// A dropped Correlation is not "unknown": forge gates its tier-2 branch on
// Correlation.ArmsRunNameMatch(), and a zero value there means "tier 2 not
// armed". Every injected trigger would go down the time-window path and come
// back `inferred` or `ambiguous` instead of `exact`.
func TestPendingCorrelationsCarriesCorrelationThrough(t *testing.T) {
	cases := []struct {
		stored   store.Correlation
		want     forge.Correlation
		wantTier bool
	}{
		{store.CorrelationInjected, forge.CorrelationInjected, true},
		{store.CorrelationDispatchResponse, forge.CorrelationDispatchResponse, false},
		{store.CorrelationWindow, forge.CorrelationWindow, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.stored), func(t *testing.T) {
			db := &stubDB{unresolved: []store.UnresolvedTrigger{{
				EventID:            "evt_0001",
				TargetProject:      "acme/core",
				TargetWorkflow:     "release.yml",
				TargetRef:          "refs/heads/main",
				TS:                 testutil.BaseTime,
				Correlation:        tc.stored,
				ExistingResolution: store.ResolutionInferred,
			}}}
			f := &ForgeStore{DB: db}

			got, err := f.PendingCorrelations(context.Background(), 5*time.Minute, 10)
			if err != nil {
				t.Fatalf("PendingCorrelations: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want one pending trigger, got %d", len(got))
			}
			p := got[0]
			if p.Correlation != tc.want {
				t.Errorf("Correlation = %q, want %q — a dropped field here reads as \"tier 2 not armed\" "+
					"and downgrades every exact link with no error anywhere",
					p.Correlation, tc.want)
			}
			// The consequence, not the field: does the resolver's tier-2 gate
			// actually open?
			if got := p.Correlation.ArmsRunNameMatch(); got != tc.wantTier {
				t.Errorf("ArmsRunNameMatch() = %v, want %v", got, tc.wantTier)
			}
			// The other half of what the resolver branches on. Without it the supersession check in Sweep
			// compares against "" and every weaker link overwrites a stronger.
			if p.ExistingResolution != forge.ResolutionInferred {
				t.Errorf("ExistingResolution = %q, want %q", p.ExistingResolution, forge.ResolutionInferred)
			}
			if p.EventID != "evt_0001" || p.Repo != "acme/core" ||
				p.Workflow != "release.yml" || p.Ref != "refs/heads/main" || !p.At.Equal(testutil.BaseTime) {
				t.Errorf("the target fields did not survive the conversion: %+v", p)
			}
		})
	}
}

// TestUnknownCorrelationIsAnErrorNotAZeroValue: the schema's CHECK means the column
// cannot legally hold anything else. If it ever does, a loud failure is correct
// and a quiet substitution is not.
func TestUnknownCorrelationIsAnErrorNotAZeroValue(t *testing.T) {
	db := &stubDB{unresolved: []store.UnresolvedTrigger{{
		EventID: "evt_0001", TargetProject: "acme/core", Correlation: store.Correlation("teleport"),
	}}}
	f := &ForgeStore{DB: db}
	if _, err := f.PendingCorrelations(context.Background(), time.Minute, 10); err == nil {
		t.Fatal("an unknown correlation was converted silently; it must be an error")
	}

	// And the empty value on the WRITE side, which is the hazard's other end.
	if _, err := Correlation(""); err == nil {
		t.Fatal("adapter.Correlation(\"\") returned a value; a correlation is required on every trigger event")
	}
	for _, c := range []forge.Correlation{
		forge.CorrelationInjected, forge.CorrelationDispatchResponse, forge.CorrelationWindow,
	} {
		if _, err := Correlation(c); err != nil {
			t.Errorf("adapter.Correlation(%q) = %v, want a conversion", c, err)
		}
	}
}

// TestInputsDefNilVersusEmptyRoundTrips: a data invariant that cannot be read
// off a signature.
//
//	""   -> nil map        the file was never read; input validation is SKIPPED
//	"{}" -> empty non-nil  the file was read and declares nothing, so any input
//	                       key the caller sends is a 422
//
// Collapsing them makes the gateway either skip validation it should perform or
// reject a legitimate request, and neither shows up as an error.
func TestInputsDefNilVersusEmptyRoundTrips(t *testing.T) {
	t.Run("SQL NULL means UNKNOWN", func(t *testing.T) {
		entry, err := WorkflowEntry(store.WorkflowCacheEntry{
			Repo: "acme/core", Workflow: "release.yml", InputsDef: "",
		})
		if err != nil {
			t.Fatalf("WorkflowEntry: %v", err)
		}
		if entry.InputsDef != nil {
			t.Fatalf("InputsDef = %#v, want nil — an unread file is UNKNOWN, not \"declares nothing\"", entry.InputsDef)
		}
		// The consequence: PlanDispatch skips validation and says so.
		plan, err := forge.PlanDispatch(
			forge.DispatchRequest{Ref: "refs/heads/main", Inputs: map[string]string{"anything": "1"}},
			entry, true, "main", forge.PlanOptions{ReturnRunDetails: true})
		if err != nil {
			t.Fatalf("PlanDispatch: %v", err)
		}
		if plan.InputsValidation != forge.InputsValidationSkippedColdCache {
			t.Errorf("inputs_validation = %q, want %q", plan.InputsValidation, forge.InputsValidationSkippedColdCache)
		}
		if plan.Rejected() {
			t.Error("an UNKNOWN definition rejected an input key; validation must be skipped, not enforced")
		}
	})

	t.Run("'{}' means DECLARES NOTHING", func(t *testing.T) {
		entry, err := WorkflowEntry(store.WorkflowCacheEntry{
			Repo: "acme/core", Workflow: "release.yml", InputsDef: "{}",
		})
		if err != nil {
			t.Fatalf("WorkflowEntry: %v", err)
		}
		if entry.InputsDef == nil {
			t.Fatal("InputsDef = nil for '{}'; the two values were collapsed")
		}
		if len(entry.InputsDef) != 0 {
			t.Fatalf("InputsDef = %#v, want an empty non-nil map", entry.InputsDef)
		}
		plan, err := forge.PlanDispatch(
			forge.DispatchRequest{Ref: "refs/heads/main", Inputs: map[string]string{"anything": "1"}},
			entry, true, "main", forge.PlanOptions{ReturnRunDetails: true})
		if err != nil {
			t.Fatalf("PlanDispatch: %v", err)
		}
		if !plan.Rejected() {
			t.Error("a workflow that declares NO inputs accepted one; input validation makes that a 422")
		}
	})

	t.Run("the write side keeps the same distinction", func(t *testing.T) {
		nilRow, err := WorkflowRow(forge.WorkflowCacheEntry{Repo: "acme/core", InputsDef: nil})
		if err != nil {
			t.Fatalf("WorkflowRow: %v", err)
		}
		if nilRow.InputsDef != "" {
			t.Errorf("a nil InputsDef became %q; it must be stored as SQL NULL", nilRow.InputsDef)
		}
		emptyRow, err := WorkflowRow(forge.WorkflowCacheEntry{
			Repo: "acme/core", InputsDef: map[string]forge.InputDef{},
		})
		if err != nil {
			t.Fatalf("WorkflowRow: %v", err)
		}
		if emptyRow.InputsDef != "{}" {
			t.Errorf("an empty InputsDef became %q, want \"{}\"", emptyRow.InputsDef)
		}
	})
}

// TestGetWorkflowCacheReportsStaleAsPresent: staleness is not coldness.
//
// ok=false means NEVER FETCHED and only that. Reporting a stale row as cold
// makes every request skip input validation — silent degradation, the number
// one failure class in this project.
func TestGetWorkflowCacheReportsStaleAsPresent(t *testing.T) {
	f := &ForgeStore{DB: &stubDB{entry: store.WorkflowCacheEntry{
		Repo: "acme/core", Workflow: "release.yml", InputsDef: `{"version":{}}`,
		// Fetched a week ago: stale by any TTL, and still usable.
		FetchedAt: testutil.BaseTime.Add(-7 * 24 * time.Hour),
	}}}
	entry, ok, err := f.GetWorkflowCache(context.Background(), "acme/core", "release.yml", "default")
	if err != nil {
		t.Fatalf("GetWorkflowCache: %v", err)
	}
	if !ok {
		t.Fatal("a STALE row was reported as cold; the workflow cache is stale-while-revalidate and cold means never fetched")
	}
	if !entry.Stale(time.Hour, testutil.BaseTime) {
		t.Error("the entry does not report itself stale, so the background refresh would never run")
	}

	// And the genuinely-never-fetched case.
	f2 := &ForgeStore{DB: &stubDB{entryErr: store.ErrNotFound}}
	if _, ok, err := f2.GetWorkflowCache(context.Background(), "acme/core", "release.yml", "default"); err != nil || ok {
		t.Errorf("a never-fetched workflow returned ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestWriteRunLinkSwallowsOnlyTheSupersessionRule: "a stronger
// link already exists" is the rule WORKING, and returning it as an error would
// make the resolver count a correct outcome as a sweep error. Every other error
// must still propagate.
func TestWriteRunLinkSwallowsOnlyTheSupersessionRule(t *testing.T) {
	weaker := &stubDB{linkErr: store.ErrWeakerResolution}
	f := &ForgeStore{DB: weaker}
	if err := f.WriteRunLink(context.Background(), forge.RunLink{
		TriggerEventID: "evt_0001", RunID: 7, Resolution: forge.ResolutionInferred,
		ResolutionSource: forge.ResolutionSourceWindow,
	}); err != nil {
		t.Errorf("ErrWeakerResolution surfaced as an error: %v", err)
	}

	boom := errors.New("disk on fire")
	f2 := &ForgeStore{DB: &stubDB{linkErr: boom}}
	if err := f2.WriteRunLink(context.Background(), forge.RunLink{
		TriggerEventID: "evt_0001", RunID: 7, Resolution: forge.ResolutionInferred,
		ResolutionSource: forge.ResolutionSourceWindow,
	}); !errors.Is(err, boom) {
		t.Errorf("a real write failure was swallowed: %v", err)
	}
}

// TestRunLinkEventRefusesALinkItCannotName: only `none` may carry no run
// id. Anything else with a missing id would claim a match it cannot name.
func TestRunLinkEventRefusesALinkItCannotName(t *testing.T) {
	for _, res := range []forge.Resolution{
		forge.ResolutionExact, forge.ResolutionInferred, forge.ResolutionAmbiguous,
	} {
		_, err := RunLinkEvent(forge.RunLink{
			TriggerEventID: "evt_0001", RunID: 0, Resolution: res,
			ResolutionSource: forge.ResolutionSourceWindow,
		})
		if err == nil {
			t.Errorf("resolution %q with no run id was accepted", res)
		}
	}
	ev, err := RunLinkEvent(forge.RunLink{
		TriggerEventID: "evt_0001", Resolution: forge.ResolutionNone,
		ResolutionSource: forge.ResolutionSourceWindow, At: testutil.BaseTime,
	})
	if err != nil {
		t.Fatalf("resolution `none` with no run id was refused: %v", err)
	}
	if ev.RunID != nil {
		t.Errorf("resolution `none` carries run_id %v, want NULL", *ev.RunID)
	}

	// The mandatory source (the schema's table-level CHECK): the zero value is not a
	// legal column value on any run_link.
	if _, err := RunLinkEvent(forge.RunLink{
		TriggerEventID: "evt_0001", RunID: 7, Resolution: forge.ResolutionExact,
	}); err == nil {
		t.Error("a run_link with no resolution_source was accepted; the schema has a CHECK against it")
	}
}
