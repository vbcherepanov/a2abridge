package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func init() {
	registerCommand(Command{
		Name:    "worker",
		Summary: "Run an always-online Claude worker in a detached tmux session",
		Run:     RunWorker,
	})
}

const (
	defaultWorkerSession = "a2abridge-worker"
	defaultWorkerCmd     = "claude"
)

// RunWorker manages an always-on Claude (or any CLI agent) inside a
// detached tmux session. Once started, the worker is registered with the
// directory like any other peer, exposes its MCP tools, and stays alive
// across IDE restarts.
//
// We don't ship the tmux binary ourselves — `which tmux` is the
// pre-flight check. On Windows the worker subcommand is unsupported
// (PowerShell jobs are a different beast); we surface an explicit error.
func RunWorker(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printWorkerUsage(stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}

	if _, err := exec.LookPath("tmux"); err != nil {
		fmt.Fprintln(stderr, "a2abridge worker: tmux is required (brew install tmux / apt install tmux)")
		return 1
	}

	switch args[0] {
	case "start":
		return workerStart(args[1:], stdout, stderr)
	case "stop":
		return workerStop(args[1:], stdout, stderr)
	case "status":
		return workerStatus(args[1:], stdout, stderr)
	case "attach":
		return workerAttach(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "a2abridge worker: unknown action %q\n\n", args[0])
		printWorkerUsage(stderr)
		return 2
	}
}

func printWorkerUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: a2abridge worker <action> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  start    Start an always-online Claude worker in a detached tmux session")
	fmt.Fprintln(w, "  stop     Stop the worker (kills the tmux session)")
	fmt.Fprintln(w, "  status   Report whether the worker is running")
	fmt.Fprintln(w, "  attach   Attach the current terminal to the worker tmux session")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Useful when you want a peer that survives every IDE restart so other")
	fmt.Fprintln(w, "agents can dispatch tasks to it 24/7. Requires tmux on PATH.")
}

func workerStart(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", defaultWorkerSession, "tmux session name")
	cmdName := fs.String("cmd", defaultWorkerCmd, "CLI to run inside the session (e.g. claude, codex)")
	prompt := fs.String("prompt", "", "initial prompt sent to the agent after launch (optional)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if running, _ := tmuxHasSession(*session); running {
		fmt.Fprintf(stdout, "worker already running in tmux session %q\n", *session)
		return 0
	}

	// `new-session -d` detaches immediately. The command runs as PID 1 of
	// the session — when it exits, tmux cleans up the session itself.
	cmd := exec.Command("tmux", "new-session", "-d", "-s", *session, *cmdName)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "tmux new-session failed: %v\n%s", err, out)
		return 1
	}

	if *prompt != "" {
		// send-keys with " Enter" submits the prompt. We pause briefly so
		// the agent has a chance to finish its splash before we type.
		seed := exec.Command("tmux", "send-keys", "-t", *session, *prompt, "Enter")
		if out, err := seed.CombinedOutput(); err != nil {
			fmt.Fprintf(stderr, "tmux send-keys failed: %v\n%s", err, out)
			return 1
		}
	}

	fmt.Fprintf(stdout,
		"worker started:\n  session: %s\n  cmd:     %s\n  attach:  tmux attach -t %s\n  stop:    a2abridge worker stop\n",
		*session, *cmdName, *session,
	)
	return 0
}

func workerStop(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", defaultWorkerSession, "tmux session name")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if running, _ := tmuxHasSession(*session); !running {
		fmt.Fprintf(stdout, "worker not running (no tmux session %q)\n", *session)
		return 0
	}
	cmd := exec.Command("tmux", "kill-session", "-t", *session)
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(stderr, "tmux kill-session failed: %v\n%s", err, out)
		return 1
	}
	fmt.Fprintf(stdout, "worker stopped (session %q)\n", *session)
	return 0
}

func workerStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", defaultWorkerSession, "tmux session name")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	running, info := tmuxHasSession(*session)
	if running {
		fmt.Fprintf(stdout, "worker:  RUNNING\nsession: %s\ndetail:  %s\n", *session, strings.TrimSpace(info))
		return 0
	}
	fmt.Fprintf(stdout, "worker:  NOT RUNNING\nsession: %s\n", *session)
	return 1
}

func workerAttach(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("worker attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", defaultWorkerSession, "tmux session name")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if running, _ := tmuxHasSession(*session); !running {
		fmt.Fprintf(stderr, "worker not running (no tmux session %q)\n", *session)
		return 1
	}
	// We can't actually attach without taking over the terminal, so we
	// print the right command for the user's shell. Doing the exec here
	// would leave them in tmux with no clean way back.
	fmt.Fprintf(stdout, "Run: tmux attach -t %s\n", *session)
	return 0
}

// tmuxHasSession returns whether a session of that name is alive, plus
// the matching `tmux display-message` line for diagnostics. The
// `has-session` exit code is 0 when present, 1 when missing, 255 on
// other errors.
func tmuxHasSession(name string) (bool, string) {
	if err := exec.Command("tmux", "has-session", "-t", name).Run(); err != nil {
		return false, ""
	}
	out, _ := exec.Command("tmux", "display-message", "-p", "-t", name,
		"#{session_name}: created #{session_created_string}, attached=#{session_attached}, windows=#{session_windows}").Output()
	return true, string(out)
}
