package exec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// The residual risk, retained: "Add a set here without an enumerator and
// you reintroduce the hole silently."
//
// internal/shadow's CHECK-literal test derives the sets it KNOWS about. A closed
// set added to this package without an enumerator cannot be compared against its
// SQL copy at all, and the CHECK-literal test stays green while gaining a blind
// spot. So this test does not iterate a list of seven — it finds the types.
//
// The rule it enforces: a named string type in this package with a `Valid() bool`
// method is a closed set, and a closed set must be able to list itself.

// TestEveryClosedSetHasAnEnumerator reads this package's own source.
func TestEveryClosedSetHasAnEnumerator(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing internal/exec: %v", err)
	}
	pkg, ok := pkgs["exec"]
	if !ok {
		t.Fatalf("package exec not found in %v", keysOf(pkgs))
	}

	stringTypes := map[string]bool{} // named string types
	hasValid := map[string]bool{}    // ... with a Valid() bool method
	enumerators := map[string]bool{} // func F() []T, keyed by T

	for _, f := range pkg.Files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if id, ok := ts.Type.(*ast.Ident); ok && id.Name == "string" {
						stringTypes[ts.Name.Name] = true
					}
				}
			case *ast.FuncDecl:
				// Method: func (x T) Valid() bool
				if d.Recv != nil && len(d.Recv.List) == 1 {
					if d.Name.Name == "Valid" && returnsBool(d) {
						hasValid[receiverType(d.Recv.List[0].Type)] = true
					}
					continue
				}
				// Package-level func F() []T with no parameters.
				if d.Type.Params != nil && len(d.Type.Params.List) != 0 {
					continue
				}
				if d.Type.Results == nil || len(d.Type.Results.List) != 1 {
					continue
				}
				arr, ok := d.Type.Results.List[0].Type.(*ast.ArrayType)
				if !ok || arr.Len != nil {
					continue
				}
				if elem, ok := arr.Elt.(*ast.Ident); ok {
					enumerators[elem.Name] = true
				}
			}
		}
	}

	if len(stringTypes) == 0 || len(hasValid) == 0 {
		t.Fatal("no named string type with a Valid() method was found; the scanner is broken and this test is vacuous")
	}

	var closed []string
	for name := range stringTypes {
		if hasValid[name] {
			closed = append(closed, name)
		}
	}
	sort.Strings(closed)
	t.Logf("closed sets found in internal/exec: %v", closed)

	for _, name := range closed {
		if !enumerators[name] {
			t.Errorf("%s is a closed set (a named string type with Valid()) and nothing in this package returns []%s.\n"+
				"A set that cannot list itself cannot be compared against its copy in shadow.db's CHECK literals, "+
				"and internal/shadow's derivation test stays green while gaining a blind spot.", name, name)
		}
	}

	// The set found by parsing must be the set the rest of the tests exercise.
	// If a type is added, this line is what makes the omission visible rather
	// than letting the table below quietly cover six of seven.
	want := []string{"Comparison", "Conclusion", "Outcome", "Purpose", "RefusalReason", "SupplyMode"}
	if strings.Join(closed, ",") != strings.Join(want, ",") {
		t.Errorf("the closed sets in this package have changed: %v (was %v).\n"+
			"Update internal/shadow's schemaVocabularies() with the new column, or state why the set has no SQL copy.", closed, want)
	}
}

func returnsBool(d *ast.FuncDecl) bool {
	if d.Type.Results == nil || len(d.Type.Results.List) != 1 {
		return false
	}
	id, ok := d.Type.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "bool"
}

func receiverType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverType(t.X)
	}
	return ""
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRunAndJobConclusionsAreNotNested — the two conclusion sets each hold a
// member the other lacks, and a caller that filters one to get the other is
// wrong in BOTH directions.
//
//   - `unmappable` is JOB-only: a run whose conclusion could not be mapped is
//     unmeasurable, not concluded.
//   - `startup_failure` is RUN-only: GitHub publishes it [V — 2026-08-23]
//     and no engine produces it.
//
// Before `startup_failure` was measured the sets were nested and this test
// asserted len(all) == len(run)+1. That relation is gone; asserting it again
// would be asserting the shape that made the run column abort on a real run.
func TestRunAndJobConclusionsAreNotNested(t *testing.T) {
	job := Conclusions()
	run := RunConclusions()

	inJob := map[Conclusion]bool{}
	for _, c := range job {
		if !c.Valid() {
			t.Errorf("Conclusions() lists %q and Valid() rejects it", c)
		}
		inJob[c] = true
	}
	inRun := map[Conclusion]bool{}
	for _, c := range run {
		if !c.IsGitHub() {
			t.Errorf("RunConclusions() lists %q and IsGitHub() rejects it", c)
		}
		inRun[c] = true
	}

	if inRun[ConclusionUnmappable] {
		t.Error("RunConclusions() admits `unmappable`; a run whose conclusion could not be mapped is unmeasurable, not concluded")
	}
	if !inJob[ConclusionUnmappable] {
		t.Error("Conclusions() dropped `unmappable`; this design's own value is what a JOB result carries when its mapping was never measured")
	}
	if !inRun[ConclusionStartupFailure] {
		t.Error("RunConclusions() does not admit `startup_failure`. GitHub emits it (1/10,897 and 7/2,604 concluded runs, measured 2026-08-23), " +
			"so a CHECK derived from this set ABORTS the insert of a run GitHub created — and a candidate GitHub created may not be dropped.")
	}
	if inJob[ConclusionStartupFailure] {
		t.Error("Conclusions() admits `startup_failure` at JOB level. The measurement is on RUNS; nothing has observed a job carrying it, " +
			"and widening Valid() also widens shadow.ValidStepConclusion, which is subtractive — that is the schema's narrowing to change, not this package's.")
	}

	// GitHub's own eight are exactly the intersection.
	eight := []Conclusion{
		ConclusionSuccess, ConclusionFailure, ConclusionCancelled, ConclusionSkipped,
		ConclusionTimedOut, ConclusionActionRequired, ConclusionNeutral, ConclusionStale,
	}
	for _, c := range eight {
		if !inJob[c] || !inRun[c] {
			t.Errorf("%q is one of GitHub's eight and must be legal at both levels (job=%v, run=%v)", c, inJob[c], inRun[c])
		}
	}
	if len(job) != len(eight)+1 || len(run) != len(eight)+1 {
		t.Errorf("Conclusions() has %d members and RunConclusions() %d; want GitHub's eight plus exactly one own value each: %v / %v", len(job), len(run), job, run)
	}

	// `in_progress` can NEVER be a member of EITHER set. It is a value of
	// GitHub's `status` field, and confusing the two was an early, expensive
	// mistake. Both predicates are checked: the two sets are separate
	// now, so checking one no longer covers the other.
	if Conclusion("in_progress").Valid() || Conclusion("in_progress").IsGitHub() {
		t.Fatal("`in_progress` is a status, never a conclusion")
	}
	if Conclusion("").Valid() || Conclusion("").IsGitHub() || Conclusion("no_such_member").IsGitHub() {
		t.Error("a conclusion predicate admits a value outside its set")
	}
}

// TestUnobservedConclusionsAreNotNarrowed guards the direction the 2026-08-23
// measurement is NOT evidence for.
//
// `timed_out`, `neutral` and `stale` were not observed in the ~13,500 runs that
// measurement covered. Absence of observation is not evidence of absence, and
// the permissive direction costs nothing: a value the set admits and GitHub
// never sends is a dead CHECK literal, while a value GitHub sends and the set
// omits aborts an INSERT. This test exists so "we have never seen one" cannot
// be turned into a deletion. It is NOT a claim that GitHub emits them.
func TestUnobservedConclusionsAreNotNarrowed(t *testing.T) {
	for _, c := range []Conclusion{ConclusionTimedOut, ConclusionNeutral, ConclusionStale} {
		if !c.IsGitHub() {
			t.Errorf("%q was dropped from the run-level set. It was never OBSERVED, which is not evidence it cannot occur; "+
				"narrowing on absence of observation is what made `startup_failure` missing in the first place.", c)
		}
		if !c.Valid() {
			t.Errorf("%q was dropped from the job-level set, on the same non-evidence.", c)
		}
	}
}

// TestRefuseRejectsAnInventedReason — a reason invented at a call site is
// precisely what the closed vocabulary forbids, and a compile-time constant cannot be checked any
// other way.
func TestRefuseRejectsAnInventedReason(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Refuse accepted a reason outside the closed set")
		}
	}()
	_ = Refuse(RefusalReason("disk_full"), nil)
}

// TestReasonOfNeverGuesses — ok=false means the error did NOT name a reason.
// Silently picking one writes a counter that is not true.
func TestReasonOfNeverGuesses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a plain error", errString("act exited 1")},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ok := ReasonOf(tc.err)
			if ok {
				t.Fatalf("ReasonOf(%v) claimed reason %q", tc.err, r)
			}
			if r != "" {
				t.Fatalf("ReasonOf returned %q with ok=false", r)
			}
		})
	}
	r, ok := ReasonOf(Refuse(RefusalTokenAbsent, errString("empty")))
	if !ok || r != RefusalTokenAbsent {
		t.Fatalf("ReasonOf lost the reason: %q, %v", r, ok)
	}
	// Wrapped, because a refusal usually arrives inside something else.
	if r, ok := ReasonOf(wrapped{Refuse(RefusalWorkdirDirty, nil)}); !ok || r != RefusalWorkdirDirty {
		t.Fatalf("ReasonOf did not unwrap: %q, %v", r, ok)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

type wrapped struct{ inner error }

func (w wrapped) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrapped) Unwrap() error { return w.inner }
