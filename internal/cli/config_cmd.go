package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/fatihy101/standpipe/internal/config"
	"github.com/fatihy101/standpipe/internal/httpapi"
)

// runConfig implements `standpipe config validate`.
func runConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("config: expected a subcommand (validate)")
	}
	switch args[0] {
	case "validate":
		return runConfigValidate(args[1:])
	default:
		return fmt.Errorf("config: unknown subcommand %q", args[0])
	}
}

// runConfigValidate has NO side effects — it is a CLI subcommand
// and deliberately not an HTTP endpoint, so validating a config file never
// exposes a YAML parser to the internet.
//
// It runs exactly the code the gateway runs at startup and on SIGHUP. That is
// the whole point: a file that validates here and then fails to load would
// make this command worse than not having it.
func runConfigValidate(args []string) error {
	fs := flag.NewFlagSet("config validate", flag.ContinueOnError)
	file := fs.String("file", "standpipe.yaml", "path to standpipe.yaml")
	alias := fs.String("config", "", "alias for -file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *file
	if *alias != "" {
		path = *alias
	}

	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stdout, "valid: false\n\n%v\n", err)
		os.Exit(1)
	}

	// Warnings never fail the run: known-future config blocks are legal and
	// the roadmap tells operators to keep them.
	for _, w := range cfg.Warnings {
		fmt.Printf("warning: %s\n", w)
	}
	// File existence is reported separately and does NOT fail validation:
	// `config validate` is routinely run from a workstation where the App
	// private key does not exist, and a false failure there teaches operators
	// to ignore this command.
	if err := cfg.CheckFiles(); err != nil {
		fmt.Printf("warning: %v\n", err)
	}

	fmt.Println("valid: true")
	fmt.Printf("listen: %s   admin_listen: %s\n", cfg.Gateway.Listen, cfg.Gateway.AdminListen)
	fmt.Printf("callers: %d   allowed_targets: %d\n", len(cfg.Callers), len(cfg.AllowedTargets))
	// The resolved value, not the raw key: an absent return_run_details means
	// tier 1 is ARMED, and an operator reading "true" here has the answer that
	// the trigger path will actually use.
	fmt.Printf("github.return_run_details (resolved): %t\n", cfg.GitHub.Tier1Armed())
	return nil
}

// runToken implements `standpipe token create`.
//
// It prints the plaintext once and the hash to paste into standpipe.yaml. The
// plaintext is never written anywhere by this process; only its hash is stored.
func runToken(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("token: expected `create`")
	}
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	caller := fs.String("caller", "", "caller name (the idempotency namespace)")
	tokenID := fs.String("token-id", "", "token id embedded in the token")
	role := fs.String("role", "caller", "caller | admin")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *caller == "" {
		return errors.New("token create: --caller is required")
	}
	if *tokenID == "" {
		return errors.New("token create: --token-id is required")
	}
	if *role != config.RoleCaller && *role != config.RoleAdmin {
		return fmt.Errorf("token create: --role must be %q or %q", config.RoleCaller, config.RoleAdmin)
	}

	token, hash, err := httpapi.MintToken(*tokenID)
	if err != nil {
		return err
	}
	fmt.Printf("token (shown once, store it now):\n\n  %s\n\n", token)
	fmt.Printf("standpipe.yaml entry:\n\n")
	fmt.Printf("callers:\n  %s:\n    token_hash: \"%s\"\n    token_id: \"%s\"\n    role: %s\n    allow:\n      - repo: <owner/name>\n        workflow: <file.yml>\n        ref: \"refs/heads/main\"\n",
		*caller, hash, *tokenID, *role)
	return nil
}
