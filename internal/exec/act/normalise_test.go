package act

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The defaults.run normalisation, and the two silent failures measured against it.

func parseBack(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("the normalised copy does not parse as YAML:\n%s\n%v", out, err)
	}
	return doc
}

func jobDefaultsRun(t *testing.T, doc map[string]any, job string) (any, bool) {
	t.Helper()
	jobs, ok := doc["jobs"].(map[string]any)
	if !ok {
		t.Fatalf("no jobs mapping in %v", doc)
	}
	j, ok := jobs[job].(map[string]any)
	if !ok {
		t.Fatalf("no job %q in %v", job, jobs)
	}
	d, ok := j["defaults"].(map[string]any)
	if !ok {
		return nil, false
	}
	v, present := d["run"]
	return v, present
}

// TestNormaliseDoesNotOrphanAnAnchor is the measured defect of 2026-08-22.
//
// The algorithm's second half deletes the workflow-level map. A job that DEFINES its own
// map by aliasing into it is left holding `run: *d` with no definition anywhere
// — YAML that does not parse. The symptom is act failing at its front door on a
// file the operator never wrote and never sees, and nobody reading "act could
// not parse the workflow" would look at the normaliser.
func TestNormaliseDoesNotOrphanAnAnchor(t *testing.T) {
	src := []byte(`name: ci
on: push
defaults:
  run: &d
    shell: bash
jobs:
  s:
    runs-on: ubuntu-24.04
    defaults:
      run: *d
    steps:
      - run: echo hi
`)
	n, err := normaliseWorkflow(src)
	if err != nil {
		t.Fatalf("normaliseWorkflow: %v", err)
	}
	if strings.Contains(string(n.text), "*d") {
		t.Fatalf("the emitted copy still carries an alias whose anchor was deleted:\n%s", n.text)
	}
	doc := parseBack(t, n.text) // this is the assertion: it must still parse

	run, present := jobDefaultsRun(t, doc, "s")
	if !present {
		t.Fatalf("the job lost its own defaults.run:\n%s", n.text)
	}
	m, ok := run.(map[string]any)
	if !ok || m["shell"] != "bash" {
		t.Fatalf("the job's effective map is %v, want {shell: bash}", run)
	}
	if _, wf := doc["defaults"]; wf {
		t.Fatalf("the workflow-level defaults survived; act's merge re-engages:\n%s", n.text)
	}
}

// TestNormaliseKeepsLiveAnchors — the orphan pass runs AFTER the edits rather
// than before, so a workflow whose anchors all survive keeps them. The
// normalised copy stays as close to the operator's text as the change allows,
// which is what makes an unexplained mismatch investigable.
func TestNormaliseKeepsLiveAnchors(t *testing.T) {
	src := []byte(`name: ci
on: push
env: &e
  FOO: bar
jobs:
  s:
    runs-on: ubuntu-24.04
    env: *e
    steps:
      - run: echo hi
`)
	n, err := normaliseWorkflow(src)
	if err != nil {
		t.Fatalf("normaliseWorkflow: %v", err)
	}
	if !strings.Contains(string(n.text), "*e") {
		t.Fatalf("a live anchor was expanded for no reason:\n%s", n.text)
	}
	parseBack(t, n.text)
	if n.applied {
		t.Error("applied is true on a workflow with no defaults.run at all")
	}
}

// TestDefinedIsThePresenceOfTheKeyNotANonEmptyMap is the InputsDef nil-versus-
// empty class exactly: a job writing `defaults: {run: {}}` DROPS the workflow map, a
// job writing no defaults.run at all INHERITS it. Collapsing them is silent and
// produces a legal-looking file.
func TestDefinedIsThePresenceOfTheKeyNotANonEmptyMap(t *testing.T) {
	src := []byte(`name: ci
on: push
defaults:
  run:
    shell: bash
    working-directory: ./app
jobs:
  drops:
    runs-on: ubuntu-24.04
    defaults:
      run: {}
    steps:
      - run: echo hi
  inherits:
    runs-on: ubuntu-24.04
    steps:
      - run: echo hi
  owns:
    runs-on: ubuntu-24.04
    defaults:
      run:
        shell: sh
    steps:
      - run: echo hi
`)
	n, err := normaliseWorkflow(src)
	if err != nil {
		t.Fatalf("normaliseWorkflow: %v", err)
	}
	doc := parseBack(t, n.text)

	run, present := jobDefaultsRun(t, doc, "drops")
	if !present {
		t.Fatalf("`defaults: {run: {}}` lost its key entirely:\n%s", n.text)
	}
	if m, ok := run.(map[string]any); !ok || len(m) != 0 {
		t.Fatalf("`defaults: {run: {}}` INHERITED the workflow map (%v); on GitHub it drops it, and the difference is silent", run)
	}

	run, present = jobDefaultsRun(t, doc, "inherits")
	if !present {
		t.Fatalf("the job with no defaults.run did not inherit the workflow map:\n%s", n.text)
	}
	m, ok := run.(map[string]any)
	if !ok || m["shell"] != "bash" || m["working-directory"] != "./app" {
		t.Fatalf("the inherited map is %v; the WHOLE map is normalised, not just `shell`", run)
	}

	run, _ = jobDefaultsRun(t, doc, "owns")
	if m, ok := run.(map[string]any); !ok || m["shell"] != "sh" || len(m) != 1 {
		t.Fatalf("a job's own map was merged with the workflow's (%v); GitHub REPLACES and act MERGES, and the merge is the divergence", run)
	}

	if _, wf := doc["defaults"]; wf {
		t.Fatalf("the workflow-level map survived; without the deletion the first half is useless:\n%s", n.text)
	}
	if !n.applied {
		t.Error("applied is false on a workflow this changed")
	}
}

// TestNormaliseIsRecordedByItsOwnSha — an unexplained mismatch cannot be
// investigated without knowing WHICH TEXT act was given, and `applied` carries
// information even when it is false: it is what gives the merge-rule
// divergence's share of the corpus.
func TestNormaliseIsRecordedByItsOwnSha(t *testing.T) {
	src := []byte(`name: ci
on: push
jobs:
  s:
    runs-on: ubuntu-24.04
    steps:
      - run: echo hi
`)
	a, err := normaliseWorkflow(src)
	if err != nil {
		t.Fatal(err)
	}
	if a.applied {
		t.Error("applied is true on a workflow with no defaults anywhere")
	}
	if len(a.sha256) != 64 {
		t.Fatalf("sha256 = %q", a.sha256)
	}

	// The sha is over the EMITTED text, not the source.
	b, err := normaliseWorkflow(a.text)
	if err != nil {
		t.Fatal(err)
	}
	if b.sha256 != a.sha256 {
		t.Fatalf("normalisation is not idempotent: %s then %s", a.sha256, b.sha256)
	}

	// A different workflow hashes differently.
	c, err := normaliseWorkflow([]byte(strings.Replace(string(src), "echo hi", "echo bye", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.sha256 == a.sha256 {
		t.Fatal("two different normalised texts share a sha")
	}
}

// TestNormaliseKeepsTheBareOnKey — the tree is edited rather than a decoded
// model precisely so that the `on:` key, which YAML 1.1 readers resolve to the
// boolean true, survives. act is a YAML 1.1-era reader on this point and a
// workflow whose trigger key became `true:` is a workflow that runs nothing.
func TestNormaliseKeepsTheBareOnKey(t *testing.T) {
	src := []byte(`name: ci
"on":
  push:
    branches: [main]
defaults:
  run:
    shell: bash
jobs:
  s:
    runs-on: ubuntu-24.04
    steps:
      - run: echo hi
`)
	n, err := normaliseWorkflow(src)
	if err != nil {
		t.Fatal(err)
	}
	text := string(n.text)
	if strings.Contains(text, "\ntrue:") {
		t.Fatalf("the trigger key was re-encoded as a boolean:\n%s", text)
	}
	if !strings.Contains(text, "on:") && !strings.Contains(text, `"on":`) {
		t.Fatalf("the trigger key is gone:\n%s", text)
	}
	// Comments and key order are preserved for the same reason.
	if !strings.Contains(text, "branches:") {
		t.Fatalf("the trigger's own body was dropped:\n%s", text)
	}
}

// TestNormaliseRefusesAWorkflowItCannotUnderstand — it must not return a text it
// silently did nothing to.
func TestNormaliseRefusesAWorkflowItCannotUnderstand(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
	}{
		{"not YAML at all", "\t- [oops\n"},
		{"not a mapping", "- a\n- b\n"},
		{"no jobs", "name: ci\non: push\n"},
		{"jobs is not a mapping", "name: ci\non: push\njobs:\n  - a\n"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := normaliseWorkflow([]byte(tc.src)); err == nil {
				t.Fatal("normaliseWorkflow accepted it and returned a text")
			}
		})
	}
}
