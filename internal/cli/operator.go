package cli

import (
	"fmt"
	"os"
)

// operatorUsage is the CLI surface minus the daemon.
//
// `standpipe gateway` does not exist: the daemon is a separate binary
// (`standpiped`, see daemon.go) so that an operator running a read-only report
// cannot start a listening server by mistyping a verb. The two are run by
// different people in different situations.
//
// `standpipe routing` is an addition to the original command table. A
// committed routing.json is part of the phase's exit condition and its format
// is specified exactly, but no other verb produces it; validation also lives
// inside `standpipe inventory` (--routing).
const operatorUsage = `standpipe — Standpipe operator CLI (Phase 0)

The gateway itself is a separate binary: standpiped.

Usage:
  standpipe config validate --file standpipe.yaml
  standpipe token create    --caller <name> --token-id <id> [--role caller|admin]
  standpipe inventory       [--config standpipe.yaml] [--human] [--routing routing.json]
  standpipe export          [--config standpipe.yaml] [--since <RFC3339>] [--out FILE]
  standpipe trace           <event-id> [--config standpipe.yaml] [--json] [--history]
  standpipe status          [--config standpipe.yaml] [--json] [--admin-url URL]
  standpipe migrate         --config standpipe.yaml [--dry-run]
  standpipe onboard         --repo <owner/repo> [--workflows a.yml,b.yml] [--yes] [--dry-run] [--signal PID]
  standpipe forget          --before <timestamp> [--dry-run]
  standpipe routing         generate|validate [--file routing.json]

Exit codes:
  standpipe status   0 when healthy; 1 on a cold workflow cache, low GitHub quota,
                     or an unresolved trigger past the resolver horizon.
                     The thresholds are fixed in Phase 0 and not configurable.
  standpipe trace    1 when an event's content_sha256 does not verify.
`

// Operator runs the `standpipe` command line and returns a process exit status.
// cmd/standpipe is a three-line wrapper around it; everything it needs lives
// here so that neither binary carries logic the other cannot see.
func Operator(args []string) int {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, operatorUsage)
		return 2
	}

	var err error
	switch args[0] {
	case "config":
		err = runConfig(args[1:])
	case "token":
		err = runToken(args[1:])
	case "migrate":
		err = runMigrate(args[1:])
	case "inventory":
		err = runInventoryCmd(args[1:])
	case "export":
		err = runExportCmd(args[1:])
	case "trace":
		err = runTraceCmd(args[1:])
	case "status":
		err = runStatusCmd(args[1:])
	case "onboard":
		err = runOnboardCmd(args[1:])
	case "forget":
		err = runForgetCmd(args[1:])
	case "routing":
		err = runRouting(args[1:])
	case "-h", "--help", "help":
		fmt.Print(operatorUsage)
		return 0
	case "gateway":
		// The verb an operator's fingers remember from before the split.
		// Saying where it went is worth more than "unknown command".
		fmt.Fprintf(os.Stderr, "standpipe: the gateway is its own binary now; run `standpiped --config standpipe.yaml`\n\n%s", operatorUsage)
		return 2
	default:
		fmt.Fprintf(os.Stderr, "standpipe: unknown command %q\n\n%s", args[0], operatorUsage)
		return 2
	}

	if err != nil {
		return reportExit("standpipe", err)
	}
	return 0
}
