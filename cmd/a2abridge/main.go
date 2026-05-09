// Command a2abridge is the multi-command binary that hosts:
//
//	a2abridge directory   — the local discovery service daemon
//	a2abridge bridge      — the per-agent MCP stdio + A2A HTTP server
//	a2abridge version     — print build info
//
// More subcommands (install, service, doctor, update, uninstall) are added
// in subsequent phases — see README.md → Roadmap.
package main

import (
	"os"

	"github.com/vbcherepanov/a2abridge/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
