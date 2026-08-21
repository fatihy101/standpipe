package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/fatihy101/standpipe/internal/config"
)

// This file is `standpipe onboard`'s write side.
//
// Onboarding states four requirements and each one is a separate failure if it is
// missed, so each is implemented separately below:
//
//   - YAML COMMENTS ARE PRESERVED. The file is edited through yaml.v3's Node
//     API rather than by unmarshalling into config.Config and marshalling back.
//     A struct round-trip is the obvious implementation and it silently deletes
//     every comment in standpipe.yaml — including the ones explaining why a
//     target is restricted to a ref.
//   - THE RESULT IS VALIDATED BEFORE IT REPLACES ANYTHING. The new text goes to
//     a temp file, through the whole of config.Parse, and only then into place.
//     `onboard` sends SIGHUP afterwards; writing a file that fails validation
//     would leave a gateway running on a configuration nobody can reload.
//   - THE REPLACEMENT IS ATOMIC. os.Rename within the same directory. A
//     truncate-and-write loses standpipe.yaml entirely if the process dies
//     mid-write, and the gateway cannot be restarted at all after that.
//   - RE-RUNNING IS IDEMPOTENT. An existing entry for the repo is UPDATED; a
//     second entry is never created. config.Validate rejects a duplicated repo
//     outright, so a non-idempotent implementation would produce a file that
//     fails its own validation on the second run.
//
// One thing yaml.v3's Node API does NOT preserve is blank lines: they are not
// nodes and do not survive the round trip. Comments do. That is a cosmetic
// loss on a file that keeps its meaning, and it is the price of not writing a
// YAML text editor.

// upsertAllowedTarget adds or updates one `allowed_targets` entry.
//
// It returns the new document and whether anything changed. An unchanged
// result is the idempotent re-run: onboarding requires the same parameters to
// produce the same outcome, and rewriting a byte-identical file would still
// bump its mtime and send a pointless SIGHUP.
func upsertAllowedTarget(raw []byte, repo string, workflows []string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("parsing the existing file: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, errors.New("standpipe.yaml is empty; onboard edits an existing configuration, it does not create one")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, false, errors.New("the top level of standpipe.yaml is not a mapping")
	}

	targets := mappingValue(root, "allowed_targets")
	if targets == nil {
		targets = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "allowed_targets"},
			targets)
	}
	if targets.Kind == yaml.ScalarNode && targets.Tag == "!!null" {
		// `allowed_targets:` with nothing under it parses as a null scalar,
		// not as an empty sequence.
		*targets = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	}
	if targets.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("allowed_targets is a %s, not a list", kindName(targets.Kind))
	}

	var entry *yaml.Node
	for _, item := range targets.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if r := mappingValue(item, "repo"); r != nil && r.Value == repo {
			entry = item
			break
		}
	}

	changed := false
	if entry == nil {
		entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "repo"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: repo})
		targets.Content = append(targets.Content, entry)
		changed = true
	}

	list := mappingValue(entry, "workflows")
	if list == nil {
		list = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "workflows"},
			list)
		changed = true
	}
	if list.Kind == yaml.ScalarNode && list.Tag == "!!null" {
		*list = yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		changed = true
	}
	if list.Kind != yaml.SequenceNode {
		return nil, false, fmt.Errorf("allowed_targets[repo=%s].workflows is a %s, not a list", repo, kindName(list.Kind))
	}

	have := map[string]bool{}
	for _, item := range list.Content {
		have[item.Value] = true
	}
	add := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		if !have[wf] {
			add = append(add, wf)
			have[wf] = true
		}
	}
	sort.Strings(add)
	for _, wf := range add {
		// Appended, never sorted into place. The operator's existing order is
		// theirs; a diff that reorders unrelated lines hides the one line that
		// actually changed.
		list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: wf})
		changed = true
	}

	if !changed {
		return raw, false, nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
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

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.SequenceNode:
		return "list"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "node"
	}
}

// writeConfigAtomically validates `next` and puts it in place of `path`.
//
// `original` is the exact bytes that were read before the edit. Just before
// the rename the file is read again and compared: if it differs, somebody else
// wrote standpipe.yaml while this command was talking to GitHub and asking the
// operator a question, and renaming over it would discard their edit with no
// trace. There is no lock on this file — the gateway itself only ever reads it
// — so a compare-before-rename is the honest amount of protection available,
// and its residual window is the microseconds between the compare and the
// rename.
func writeConfigAtomically(path string, original, next []byte) error {
	// Validate FIRST, and through the same code the gateway runs (`standpipe config validate`). A
	// config that only this command accepts is worse than no validation.
	if _, err := config.Parse(next); err != nil {
		return fmt.Errorf("the edited configuration does not validate, so it was NOT written:\n%w", err)
	}

	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".standpipe-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(next); err != nil {
		_ = tmp.Close()
		return err
	}
	// fsync before rename: the rename can otherwise be durable while the
	// content behind it is not, which on a crash leaves an empty standpipe.yaml
	// and a gateway that cannot start.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}

	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("%s changed on disk while onboard was running; nothing was written.\n"+
			"        Another edit (an operator, or a second `standpipe onboard`) landed in between.\n"+
			"        Re-run the command against the current file", path)
	}

	return os.Rename(tmpName, path)
}
