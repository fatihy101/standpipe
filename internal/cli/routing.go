package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// routing.json's parser lives here, in internal/cli, and that placement is
// the implementation of the file's central rule: the gateway does not read
// this file. The consumer is the CALLER, reading a local clone during an
// outage (the direct-call runbook) — independent readability is the entire
// point, and a gateway that started depending on the file would make the
// direct-call path depend on the thing that is down.
//
// Stating that as a rule protects nothing; a later slice adds one import and
// the rule is gone with no symptom. So the placement is what enforces it:
// internal/cli imports all six library packages — httpapi, forge, store,
// config, metrics and adapter — so none of them can import internal/cli back.
// That would be an import cycle, and the compiler refuses it. The request path
// cannot read routing.json even by mistake.
//
// READ THE MECHANISM CAREFULLY, IT IS WEAKER THAN IT USED TO BE. Until the
// Standpipe rename (2026-08-21) this parser was cmd/standby/routing.go in
// package main, and the enforcement was that Go forbids importing a main
// package at all. That was STRUCTURAL: no arrangement of the library packages
// could have reached it. What holds today is only ACYCLICITY, and acyclicity is
// CONTINGENT on internal/cli remaining the package that imports the gateway
// packages. Split internal/cli so that a routing-only package imports nothing,
// or move the parser to a package the gateway can reach, and the cycle — and
// with it the guarantee — is gone. The conclusion survived the rename; the
// reason did not, and a reader who checks the old reason, finds it false, and
// concludes the boundary is gone is one step from a violation that compiles.
//
// That violation is not hypothetical. Moving LoadRouting into internal/config —
// the plausible "config parsing belongs in config" refactor — builds and vets
// clean, and every gateway package silently gains the ability the documents say
// it does not have.
//
// internal/cli/routing_boundary_test.go is what makes the claim checkable. It
// parses the module's own import graph, finds the parser's package by following
// LoadRouting/RoutingFile rather than by naming internal/cli, and fails when any
// non-main package can reach it. It is the only thing that catches the move
// described above. If that test is deleted or re-pointed at a hardcoded package,
// this comment is back to being a convention.
//
// The corollary that keeps this true: this file must stay free of anything the
// gateway needs, or somebody will be tempted to move it into a package the
// gateway imports.

// routingSchemaVersion is the file's `schema_version`. It becomes 2 in Phase 2 when
// `overrides` gains routing semantics; in Phase 0 anything else is a refusal,
// because a caller reading a version it does not understand must stop rather
// than guess a backend.
const routingSchemaVersion = 1

// routingDefaultBackend is the only legal `default_backend` in Phase 0.
const routingDefaultBackend = "github"

// RoutingFile is the routing.json document.
//
// Field order here is the order it is written in, and it matches the
// documented example. The file is read by humans during an incident.
type RoutingFile struct {
	SchemaVersion  int               `json:"schema_version"`
	DefaultBackend string            `json:"default_backend"`
	UpdatedAt      string            `json:"updated_at"`
	UpdatedBy      string            `json:"updated_by"`
	Reason         string            `json:"reason"`
	Overrides      map[string]string `json:"overrides"`
}

// NewRoutingFile builds the Phase 0 document.
func NewRoutingFile(updatedBy, reason string, now time.Time) RoutingFile {
	if reason == "" {
		reason = "initial_observe_mode"
	}
	return RoutingFile{
		SchemaVersion:  routingSchemaVersion,
		DefaultBackend: routingDefaultBackend,
		UpdatedAt:      now.UTC().Format(time.RFC3339),
		UpdatedBy:      updatedBy,
		Reason:         reason,
		// Non-nil: `"overrides": {}` is a statement that there are none, and
		// `null` would make a caller's `overrides[workflow]` lookup a decision
		// about a missing key rather than an empty map.
		Overrides: map[string]string{},
	}
}

// ValidateRouting checks a routing.json document against its format and returns
// EVERY problem, not the first.
//
// It is deliberately strict about `overrides`: Phase 0 requires it empty, and a
// non-empty one means somebody wrote Phase 2 routing into a file that no Phase 0
// component will act on. That is a routing rule which silently does nothing —
// exactly the silent-success failure mode this project forbids.
func ValidateRouting(f RoutingFile, raw map[string]any) []string {
	var errs []string
	if f.SchemaVersion != routingSchemaVersion {
		errs = append(errs, fmt.Sprintf("schema_version is %d; Phase 0 defines %d", f.SchemaVersion, routingSchemaVersion))
	}
	if f.DefaultBackend != routingDefaultBackend {
		errs = append(errs, fmt.Sprintf("default_backend is %q; in Phase 0 it is always %q", f.DefaultBackend, routingDefaultBackend))
	}
	if f.UpdatedAt == "" {
		errs = append(errs, "updated_at is required (RFC 3339 UTC)")
	} else if _, err := time.Parse(time.RFC3339, f.UpdatedAt); err != nil {
		errs = append(errs, fmt.Sprintf("updated_at %q is not RFC 3339", f.UpdatedAt))
	}
	if f.UpdatedBy == "" {
		errs = append(errs, "updated_by is required")
	}
	if f.Reason == "" {
		errs = append(errs, "reason is required")
	}
	if len(f.Overrides) > 0 {
		keys := make([]string, 0, len(f.Overrides))
		for k := range f.Overrides {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		errs = append(errs, fmt.Sprintf(
			"overrides must be empty in Phase 0 but has %d entr(ies) (%v); Phase 0 makes no routing decision, so these would be read by nobody",
			len(keys), keys))
	}
	if raw != nil {
		if _, ok := raw["overrides"]; !ok {
			errs = append(errs, "overrides is missing; write `\"overrides\": {}` so a caller's lookup has an empty map rather than null")
		}
		known := map[string]bool{
			"schema_version": true, "default_backend": true, "updated_at": true,
			"updated_by": true, "reason": true, "overrides": true,
		}
		var unknown []string
		for k := range raw {
			if !known[k] {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		for _, k := range unknown {
			errs = append(errs, fmt.Sprintf("unknown field %q; the field list is closed", k))
		}
	}
	return errs
}

// LoadRouting reads and parses routing.json. It returns the decoded document
// and the raw object, because the unknown-field and missing-key checks cannot
// be made against a struct.
func LoadRouting(path string) (RoutingFile, map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return RoutingFile{}, nil, err
	}
	var f RoutingFile
	if err := json.Unmarshal(b, &f); err != nil {
		return RoutingFile{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return RoutingFile{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, raw, nil
}

// runRouting implements `standpipe routing generate` and `standpipe routing
// validate`.
//
// The exit condition of Phase 0 requires routing.json to be in the
// repository with exactly specified content, but no other verb produces the
// file. This verb fills that hole; the same validator is reachable from
// `standpipe inventory --routing`.
func runRouting(args []string) error {
	if len(args) == 0 {
		return errors.New("routing: expected a subcommand (generate | validate)")
	}
	switch args[0] {
	case "generate":
		return runRoutingGenerate(args[1:])
	case "validate":
		return runRoutingValidate(args[1:])
	default:
		return fmt.Errorf("routing: unknown subcommand %q (generate | validate)", args[0])
	}
}

func runRoutingGenerate(args []string) error {
	fs := newFlagSet("routing generate")
	path := fs.String("file", "routing.json", "where to write routing.json (the repository root)")
	updatedBy := fs.String("updated-by", "", "who is making this change")
	reason := fs.String("reason", "initial_observe_mode", "why")
	force := fs.Bool("force", false, "overwrite an existing file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *updatedBy == "" {
		// Not defaulted to $USER. updated_by is read during an incident
		// to find the person who made the change; a machine account name
		// picked up from the environment answers the question wrongly.
		return errors.New("routing generate: --updated-by is required")
	}
	if _, err := os.Stat(*path); err == nil && !*force {
		return fmt.Errorf("routing generate: %s already exists; pass --force to replace it", *path)
	}

	f := NewRoutingFile(*updatedBy, *reason, time.Now())
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	// Written through a temp file and renamed. routing.json is what a caller
	// reads when the gateway is unreachable; a half-written one during
	// an outage is worse than an old one.
	dir := filepath.Dir(*path)
	tmp, err := os.CreateTemp(dir, ".routing-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, *path); err != nil {
		return err
	}

	fmt.Printf("wrote %s\n", *path)
	fmt.Printf("  schema_version %d · default_backend %q · overrides {}\n", f.SchemaVersion, f.DefaultBackend)
	fmt.Printf("\nCommit it to the repository root. The caller reads it from a LOCAL\n")
	fmt.Printf("clone: a GitHub raw URL is unreachable in exactly the outage it exists for.\n")
	return nil
}

func runRoutingValidate(args []string) error {
	fs := newFlagSet("routing validate")
	path := fs.String("file", "routing.json", "path to routing.json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, raw, err := LoadRouting(*path)
	if err != nil {
		fmt.Printf("valid: false\n\n%v\n", err)
		return silentExit(1)
	}
	problems := ValidateRouting(f, raw)
	if len(problems) > 0 {
		fmt.Printf("valid: false\n\n")
		for _, p := range problems {
			fmt.Printf("  - %s\n", p)
		}
		return silentExit(1)
	}
	fmt.Printf("valid: true\n")
	fmt.Printf("%s: schema_version %d · default_backend %q · updated_at %s · updated_by %s\n",
		*path, f.SchemaVersion, f.DefaultBackend, f.UpdatedAt, f.UpdatedBy)
	return nil
}
