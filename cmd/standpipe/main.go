// Command standpipe is the Standpipe operator CLI.
//
// It reports on and maintains a gateway's database and configuration; it never
// serves traffic. The daemon is cmd/standpiped. Everything both binaries need
// lives in internal/cli.
package main

import (
	"os"

	"github.com/fatihy101/standpipe/internal/cli"
)

func main() { os.Exit(cli.Operator(os.Args[1:])) }
