package cli

import "fmt"

// daemonUsage is the whole of `standpiped`. It serves the gateway and does
// nothing else, so it takes flags directly rather than a verb.
const daemonUsage = `standpiped — Standpipe gateway daemon (Phase 0)

Phase 0 runs in observation mode: authenticate, apply idempotency, authorize,
forward the request to GitHub unchanged, and record everything. It makes no
routing decisions and knows no second backend. See docs/architecture.md.

Usage:
  standpiped --config standpipe.yaml [--listen :8443] [--admin-listen 127.0.0.1:9090] [--pid-file PATH]

Operator commands (inventory, trace, status, export, onboard, forget, migrate,
token, config, routing) live in the standpipe binary.
`

// Daemon runs `standpiped` and returns a process exit status.
func Daemon(args []string) int {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			fmt.Print(daemonUsage)
			return 0
		}
	}
	if err := runGateway(args); err != nil {
		return reportExit("standpiped", err)
	}
	return 0
}
