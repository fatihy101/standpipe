package harness

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// (d) — event payload completeness.
//
// WHY IT EXISTS. (c)'s miss is CAUSED by the engine running with an empty
// `github.event` [M — corpus run g11]. Under workflow_dispatch the engine runs
// with an empty event object, `github.event.repository` is null, an `if:` that
// reads through it evaluates FALSE, and the guarded steps vanish from the log
// entirely — not recorded as skipped, NOT RECORDED AT ALL. GitHub ran both and
// both passed. Every job produced a result; there was not one skip line.
//
// (d) catches that BEFORE a run is spent; (c) catches the case where the
// payload looked complete and a step disappeared anyway. The overlap is
// deliberate: (d) is about the payload WE produced, (c) is about what the
// engine did with it.

// Reference is one `github.event.*` or `inputs.*` path found in the workflow.
type Reference struct {
	// Root is "github.event" or "inputs".
	Root string
	// Path is the accessor chain after the root, in order.
	Path []string
	// Text is the reference as written, for the operator who has to fix it.
	Text string
	// Source names where the model found it — "job.build.if",
	// "job.build.step.2.if", "env", ... Diagnostic only.
	Source string
}

func (r Reference) String() string {
	if r.Source == "" {
		return r.Text
	}
	return r.Text + " (" + r.Source + ")"
}

// References collects the scan of a whole workflow.
type References []Reference

// ScanModel extracts every `github.event.*` and `inputs.*` reference the model
// carries (the payload check's first step).
//
// GUARDS ARE SCANNED AS WHOLE EXPRESSIONS, everything else only inside ${{ }}.
// GitHub allows `if:` without the ${{ }} wrapper, and
// `if: github.event.repository.private == true` — g11's own guard — has none.
// A scanner that only looked between braces would pass g11 unread, which is
// precisely the failure (d) was written for.
func ScanModel(m Model) References {
	var out References
	for _, e := range m.Expressions {
		out = append(out, scanTemplate(e, "expression")...)
	}
	for _, j := range m.Jobs {
		if j.If != "" {
			out = append(out, scanExpression(j.If, "job."+j.JobID+".if")...)
		}
		for i, s := range j.Steps {
			if s.If != "" {
				out = append(out, scanExpression(s.If, "job."+j.JobID+".step."+strconv.Itoa(i)+".if")...)
			}
		}
	}
	return out
}

// Unresolved returns the references that do NOT resolve inside payload.
//
// The payload check's second step: a single unresolved path and the run is refused BEFORE it is
// spent — `refused` / `event_payload_incomplete` (row 2).
//
// A path that resolves to an explicit JSON null at its LEAF is resolved: the
// key exists and GitHub sends nulls. A null in the MIDDLE of a path is not —
// there is nothing to descend into, and that is g11 exactly
// (`repository: null`, then `.private`).
func Unresolved(payload []byte, refs References) (References, error) {
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("harness: event payload is not JSON: %w", err)
	}
	obj, isObj := root.(map[string]any)
	if !isObj {
		return nil, fmt.Errorf("harness: event payload is not a JSON object")
	}

	var bad References
	seen := map[string]bool{}
	for _, r := range refs {
		if seen[r.Root+"|"+strings.Join(r.Path, "\x00")] {
			continue
		}
		seen[r.Root+"|"+strings.Join(r.Path, "\x00")] = true

		var start any
		switch r.Root {
		case "github.event":
			// A bare `github.event` — toJSON(github.event), for instance —
			// resolves only against a NON-EMPTY object. `{}` passes
			// exec.Request.Validate (it is not zero bytes) and is the exact
			// payload that produced g11.
			if len(r.Path) == 0 {
				if len(obj) == 0 {
					bad = append(bad, r)
				}
				continue
			}
			start = root
		case "inputs":
			// workflow_dispatch inputs arrive under event.inputs. A reusable
			// workflow's `inputs` context is a different thing and is not
			// modelled here; input-bearing dispatch runs are excluded from
			// the drill anyway, so the strict reading is the safe one.
			v, ok := obj["inputs"]
			if !ok || v == nil {
				bad = append(bad, r)
				continue
			}
			start = v
		default:
			continue
		}
		if !resolve(start, r.Path) {
			bad = append(bad, r)
		}
	}
	return bad, nil
}

// resolve walks path from v. It returns false the moment a segment cannot be
// descended into.
func resolve(v any, path []string) bool {
	cur := v
	for _, seg := range path {
		if seg == "*" {
			// An array wildcard. The path is treated as resolved up to here
			// rather than being followed into every element: refusing on a
			// wildcard would reject workflows (d) has no evidence against, and
			// the direction of a false refusal is a run the gate never gets.
			return true
		}
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[seg]
			if !ok {
				return false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(t) {
				return false
			}
			cur = t[i]
		default:
			// nil included: there is nothing to descend into. This is the g11
			// branch.
			return false
		}
	}
	return true
}

// FieldSet flattens a payload into the set of paths it carries — the payload
// check's post-run assertion compares FIELD SETS, not values, because the values
// differ by construction (timestamps, ids).
//
// Array elements collapse onto a single `[]` segment so that two payloads
// differing only in how many commits they carry are not reported as differing
// in shape.
func FieldSet(payload []byte) (map[string]bool, error) {
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("harness: payload is not JSON: %w", err)
	}
	out := map[string]bool{}
	flatten(root, "", out)
	return out, nil
}

func flatten(v any, prefix string, out map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			flatten(sub, p, out)
		}
	case []any:
		p := prefix + "[]"
		if len(t) > 0 {
			out[p] = true
		}
		for _, sub := range t {
			flatten(sub, p, out)
		}
	}
}

// FieldSetDiff reports the paths GitHub's payload carries that ours does not,
// and the reverse. Both lists sorted, for a stable record.
func FieldSetDiff(ours, theirs map[string]bool) (missingFromOurs, extraInOurs []string) {
	for p := range theirs {
		if !ours[p] {
			missingFromOurs = append(missingFromOurs, p)
		}
	}
	for p := range ours {
		if !theirs[p] {
			extraInOurs = append(extraInOurs, p)
		}
	}
	sort.Strings(missingFromOurs)
	sort.Strings(extraInOurs)
	return missingFromOurs, extraInOurs
}

// scanTemplate finds ${{ ... }} regions and scans each as an expression.
func scanTemplate(text, source string) References {
	var out References
	rest := text
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			return out
		}
		rest = rest[i+3:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			// Unterminated. Scan what is there rather than dropping it: a
			// dropped expression is a reference (d) never checks.
			out = append(out, scanExpression(rest, source)...)
			return out
		}
		out = append(out, scanExpression(rest[:j], source)...)
		rest = rest[j+2:]
	}
}

// scanExpression walks one expression and collects every context reference
// rooted at `github.event` or `inputs`.
//
// It is hand-rolled rather than a regexp for two reasons that both cost
// correctness:
//
//   - `github.event_name` and `github.event_path` are extremely common and
//     must NOT match `github.event`. Go's RE2 has no lookahead, so the
//     boundary cannot be expressed as `github\.event(?![A-Za-z0-9_])`; the
//     obvious pattern silently treats `github.event_name` as a reference to
//     `github.event` and then reports it unresolved on every payload.
//   - String literals must be skipped, or `contains(x, 'github.event.foo')`
//     becomes a reference to a path nobody wrote.
func scanExpression(expr, source string) References {
	var out References
	s := []rune(expr)
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\'' || c == '"' {
			i = skipString(s, i)
			continue
		}
		if !isIdentStart(c) {
			i++
			continue
		}
		// A dotted accessor cannot start an identifier we care about, and a
		// preceding '.' means we are mid-chain of something else.
		if i > 0 && (s[i-1] == '.' || isIdentPart(s[i-1])) {
			i = skipIdent(s, i)
			continue
		}
		start := i
		root, next := readIdent(s, i)
		i = next
		switch root {
		case "inputs":
			path, end := readAccessors(s, i)
			out = append(out, Reference{Root: "inputs", Path: path, Text: string(s[start:end]), Source: source})
			i = end
		case "github":
			path, end := readAccessors(s, i)
			if len(path) > 0 && path[0] == "event" {
				out = append(out, Reference{Root: "github.event", Path: path[1:], Text: string(s[start:end]), Source: source})
			}
			i = end
		}
	}
	return out
}

func skipString(s []rune, i int) int {
	q := s[i]
	i++
	for i < len(s) {
		if s[i] == q {
			// '' is GitHub's escape for a single quote inside a literal.
			if q == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return len(s)
}

func isIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c rune) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-'
}

func skipIdent(s []rune, i int) int {
	for i < len(s) && isIdentPart(s[i]) {
		i++
	}
	return i
}

func readIdent(s []rune, i int) (string, int) {
	start := i
	for i < len(s) && isIdentPart(s[i]) {
		i++
	}
	return string(s[start:i]), i
}

// readAccessors reads the `.name` / `['name']` / `[0]` / `.*` chain after a
// root identifier.
func readAccessors(s []rune, i int) ([]string, int) {
	var path []string
	for {
		j := skipSpace(s, i)
		if j >= len(s) {
			return path, i
		}
		switch s[j] {
		case '.':
			k := skipSpace(s, j+1)
			if k < len(s) && s[k] == '*' {
				path = append(path, "*")
				i = k + 1
				continue
			}
			if k >= len(s) || !isIdentStart(s[k]) {
				return path, i
			}
			name, end := readIdent(s, k)
			path = append(path, name)
			i = end
		case '[':
			k := skipSpace(s, j+1)
			if k >= len(s) {
				return path, i
			}
			var seg string
			var end int
			switch {
			case s[k] == '\'' || s[k] == '"':
				q := s[k]
				m := k + 1
				var b strings.Builder
				for m < len(s) && s[m] != q {
					b.WriteRune(s[m])
					m++
				}
				seg, end = b.String(), m+1
			case s[k] >= '0' && s[k] <= '9':
				m := k
				for m < len(s) && s[m] >= '0' && s[m] <= '9' {
					m++
				}
				seg, end = string(s[k:m]), m
			default:
				// A computed index — `github.event[matrix.key]`. Not
				// resolvable statically; the chain stops here rather than
				// being guessed.
				return path, i
			}
			end = skipSpace(s, end)
			if end >= len(s) || s[end] != ']' {
				return path, i
			}
			path = append(path, seg)
			i = end + 1
		default:
			return path, i
		}
	}
}

func skipSpace(s []rune, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}
