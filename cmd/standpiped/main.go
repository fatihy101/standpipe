// Command standpiped is the Standpipe gateway daemon.
//
// Phase 0 runs in observation mode: authenticate, apply idempotency, authorize,
// forward the request to GitHub unchanged, and record everything. It makes no
// routing decisions and knows no second backend. See docs/architecture.md.
//
// It has no operator subcommands; those are cmd/standpipe. Everything both
// binaries need lives in internal/cli.
package main

import (
	"os"

	"github.com/fatihy101/standpipe/internal/cli"
)

func main() { os.Exit(cli.Daemon(os.Args[1:])) }
