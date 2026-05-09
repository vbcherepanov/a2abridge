// Package cli is the dispatch layer for the a2abridge multi-command binary.
package cli

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/vbcherepanov/a2abridge/internal/buildinfo"
)

// Command is a single subcommand registered with the dispatcher.
type Command struct {
	Name    string
	Summary string
	// Run executes the subcommand with the args following the subcommand name.
	// stdout/stderr are passed in for testability.
	Run func(args []string, stdout, stderr io.Writer) int
}

// commands is populated by registerCommand from package-level init blocks.
// Files like service.go, install.go, doctor.go can self-register without
// touching this central file.
var commands = map[string]Command{}

// registerCommand adds c to the dispatcher. Panics if a duplicate Name is
// registered — that's a programmer error caught at startup.
func registerCommand(c Command) {
	if _, exists := commands[c.Name]; exists {
		panic("a2abridge: duplicate subcommand registered: " + c.Name)
	}
	commands[c.Name] = c
}

func init() {
	registerCommand(Command{Name: "directory", Summary: "Run the local A2A discovery service", Run: RunDirectory})
	registerCommand(Command{Name: "bridge", Summary: "Run as MCP stdio server (used by IDEs)", Run: RunBridge})
	registerCommand(Command{Name: "version", Summary: "Print version and build info", Run: RunVersion})
}

func registry() map[string]Command {
	out := make(map[string]Command, len(commands))
	for k, v := range commands {
		out[k] = v
	}
	return out
}

// Run is the entry point used by cmd/a2abridge/main.go. It returns an exit code.
func Run(args []string) int {
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		if len(args) >= 2 {
			return printSubcommandHelp(args[1], stdout, stderr)
		}
		printUsage(stdout)
		return 0
	case "-v", "--version":
		return RunVersion(nil, stdout, stderr)
	}

	reg := registry()
	cmd, ok := reg[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "a2abridge: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
	return cmd.Run(args[1:], stdout, stderr)
}

func printUsage(w io.Writer) {
	reg := registry()
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)

	var b strings.Builder
	fmt.Fprintf(&b, "a2abridge %s — A2A 1.0 protocol bridge for AI coding agents\n\n", buildinfo.Version)
	b.WriteString("Usage:\n")
	b.WriteString("  a2abridge <command> [flags]\n\n")
	b.WriteString("Commands:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %-12s  %s\n", n, reg[n].Summary)
	}
	b.WriteString("\nUse \"a2abridge help <command>\" for command-specific flags.\n")
	_, _ = io.WriteString(w, b.String())
}

func printSubcommandHelp(name string, stdout, stderr io.Writer) int {
	reg := registry()
	cmd, ok := reg[name]
	if !ok {
		fmt.Fprintf(stderr, "a2abridge: unknown subcommand %q\n", name)
		return 2
	}
	// Subcommand prints its own -h via flag.FlagSet on the "-h" arg.
	return cmd.Run([]string{"-h"}, stdout, stderr)
}
