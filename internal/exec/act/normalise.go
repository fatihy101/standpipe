package act

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

// defaults.run normalisation. This is U-2: act's own semantics are
// not what gets measured; GitHub's are.
//
// THE DIVERGENCE. When a job declares its own defaults.run, GitHub REPLACES the
// workflow-level map entirely. act MERGES it key by key. Measured: in one test workflow
// GitHub falls back to `bash -e` without pipefail and FAILS, act uses
// `bash -e -o pipefail` and PASSES; a second workflow isolated it — with no job-level
// defaults.run the two sides agree, so the divergence is exactly the merge rule
// [M].
//
// Direction: the dangerous one. act passes where GitHub fails. This class may
// not be excluded from the corpus, because it is act claiming a success it has
// not earned and that is the whole reason the gate exists.
//
// THE ALGORITHM DOES TWO THINGS AND BOTH ARE REQUIRED:
//
//	for each job J:  J.defaults.run = J's own map if it HAS one, else the
//	                 workflow's map
//	then:            delete the WORKFLOW-level map
//
// Without the deletion, act's merge would still engage against a workflow-level
// map that is still there.
//
// UNDEFINED IS NOT THE EMPTY MAP. A job writing `defaults: {run: {}}` DROPS the
// workflow map on GitHub; a job writing no defaults.run at all INHERITS it.
// This is the same class as InputsDef nil versus empty map, and it fails the same way — silently, with a legal-looking value.
//
// The whole map is normalised, not just `shell`. `working-directory` is subject
// to the same rule; that it behaves the same way is UNMEASURED.
//
// It is done on a COPY. The operator's file is never written: a rehearsal does
// not change the thing it is rehearsing. The model is
// unaffected — the plan comes from the ORIGINAL model, so a defect in
// normalisation stays visible through the plan.

type normalisation struct {
	text    []byte
	sha256  string
	applied bool
}

// normaliseWorkflow rewrites a workflow's defaults.run per the algorithm
// above.
//
// It edits the parsed yaml.Node tree rather than a decoded model so that
// everything it is not about — key order, comments, block styles, and the `on:`
// key, which some YAML readers resolve to the boolean true — survives
// byte-for-byte where it can.
func normaliseWorkflow(src []byte) (normalisation, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return normalisation{}, fmt.Errorf("parsing workflow: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return normalisation{}, fmt.Errorf("workflow is not a mapping")
	}

	// D_w — the workflow-level map. Absent means the empty map.
	wfDefaults := mapValue(root, "defaults")
	var dw *yaml.Node
	if wfDefaults != nil && wfDefaults.Kind == yaml.MappingNode {
		dw = mapValue(wfDefaults, "run")
	}

	jobs := mapValue(root, "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return normalisation{}, fmt.Errorf("workflow has no jobs mapping")
	}

	applied := false
	for i := 0; i+1 < len(jobs.Content); i += 2 {
		job := jobs.Content[i+1]
		if job.Kind != yaml.MappingNode {
			continue
		}
		jobDefaults := mapValue(job, "defaults")
		// D_j is UNDEFINED when the `run` key is not present at all. A present
		// key with an empty or null value is DEFINED-and-empty and drops D_w.
		defined := jobDefaults != nil && jobDefaults.Kind == yaml.MappingNode && mapValue(jobDefaults, "run") != nil

		if defined {
			// replace: the job's own map stands, the workflow's falls away
			// entirely. Nothing to write — deleting D_w below is what makes it
			// true.
			continue
		}
		if dw == nil {
			continue // nothing to inherit
		}
		// inherit: write the effective map into the job.
		setJobDefaultsRun(job, deepCopy(dw))
		applied = true
	}

	// Delete the workflow-level map. Without this act's merge re-engages.
	if dw != nil {
		deleteKey(wfDefaults, "run")
		if len(wfDefaults.Content) == 0 {
			deleteKey(root, "defaults")
		}
		applied = true
	}

	// Deleting a subtree can ORPHAN an alias that pointed into it. Measured on
	// 2026-08-22 against a workflow whose job wrote `defaults: {run: *d}`
	// against a workflow-level `run: &d`: the anchor is deleted, the alias
	// survives, and the emitted file carries `run: *d` with no definition
	// anywhere — YAML that does not parse. The failure is at act's front door
	// and would read as "act cannot read our workflow", not as a normalisation
	// defect.
	resolveOrphanedAliases(root)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return normalisation{}, fmt.Errorf("re-encoding workflow: %w", err)
	}
	if err := enc.Close(); err != nil {
		return normalisation{}, fmt.Errorf("re-encoding workflow: %w", err)
	}
	out := buf.Bytes()

	sum := sha256.Sum256(out)
	return normalisation{
		text: out,
		// The SHA-256 of the copy is recorded, because an unexplained
		// mismatch cannot be investigated without knowing WHICH TEXT act was
		// given.
		sha256: hex.EncodeToString(sum[:]),
		// Whether normalisation changed anything is recorded too. A field
		// that is always zero still carries information — it is what gives
		// the merge-rule divergence's share of the corpus.
		applied: applied,
	}, nil
}

func documentRoot(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		return n.Content[0]
	}
	if n.Kind == 0 {
		return nil
	}
	return n
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func deleteKey(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func setKey(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val)
}

func setJobDefaultsRun(job *yaml.Node, run *yaml.Node) {
	defaults := mapValue(job, "defaults")
	if defaults == nil || defaults.Kind != yaml.MappingNode {
		defaults = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setKey(job, "defaults", defaults)
	}
	setKey(defaults, "run", run)
}

// deepCopy copies a node, RESOLVING aliases as it goes.
//
// Resolution is not tidiness. The workflow-level defaults.run is deleted at the
// end, so a job that merely aliased it would be left pointing at an anchor that
// no longer appears in the document — an alias with no definition, which is not
// valid YAML. Copying the target instead makes the effective map stand on its
// own in every job, which is exactly what the algorithm asks for.
func deepCopy(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return deepCopy(n.Alias)
	}
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = deepCopy(child)
	}
	// A copied anchor would be a second definition of the same name in one
	// document.
	c.Anchor = ""
	c.Alias = nil
	return &c
}

// resolveOrphanedAliases replaces every alias whose anchor is no longer part of
// the document with a copy of what it used to point at.
//
// It runs AFTER the edits rather than before, so a workflow whose anchors are
// all still reachable comes out with its anchors intact — the normalised copy
// stays as close to the operator's text as the change allows, which is what
// makes an unexplained mismatch investigable.
func resolveOrphanedAliases(root *yaml.Node) {
	live := map[*yaml.Node]bool{}
	var mark func(n *yaml.Node)
	mark = func(n *yaml.Node) {
		if n == nil || live[n] {
			return
		}
		live[n] = true
		for _, c := range n.Content {
			mark(c)
		}
	}
	mark(root)

	var fix func(n *yaml.Node)
	fix = func(n *yaml.Node) {
		if n == nil {
			return
		}
		for i, c := range n.Content {
			if c != nil && c.Kind == yaml.AliasNode && c.Alias != nil && !live[c.Alias] {
				n.Content[i] = deepCopy(c.Alias)
				continue
			}
			fix(c)
		}
	}
	fix(root)
}
