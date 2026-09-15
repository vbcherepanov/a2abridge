package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/vbcherepanov/a2abridge/v4/internal/buildinfo"
	"github.com/vbcherepanov/a2abridge/v4/internal/security"
)

const (
	defaultDirectoryAddr = "127.0.0.1:7777"
	serviceName          = "a2abridge-directory"
	serviceDisplay       = "a2abridge directory"
	serviceDescription   = "Local A2A 1.0 discovery service for AI coding agents."
)

// init registers the "service" subcommand. Done in init so we don't have to
// touch the central registry whenever a new subcommand is added — each file
// can self-register.
func init() {
	registerCommand(Command{
		Name:    "service",
		Summary: "Manage the directory daemon (launchd / systemd-user / Windows Service)",
		Run:     RunService,
	})
}

// RunService dispatches a "service" subaction.
func RunService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printServiceUsage(stderr)
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		printServiceUsage(stdout)
		return 0
	}
	action := args[0]
	rest := args[1:]

	switch action {
	case "install":
		return svcInstall(rest, stdout, stderr)
	case "uninstall", "remove":
		return svcAction("uninstall", rest, stdout, stderr)
	case "start":
		return svcAction("start", rest, stdout, stderr)
	case "stop":
		return svcAction("stop", rest, stdout, stderr)
	case "restart":
		return svcAction("restart", rest, stdout, stderr)
	case "status":
		return svcStatus(rest, stdout, stderr)
	case "run":
		// Hidden: invoked by the OS supervisor, not by humans.
		return svcRun(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "a2abridge service: unknown action %q\n\n", action)
		printServiceUsage(stderr)
		return 2
	}
}

func printServiceUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: a2abridge service <action> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Actions:")
	fmt.Fprintln(w, "  install     Register the directory daemon with the OS supervisor and start it")
	fmt.Fprintln(w, "  uninstall   Unregister the daemon (alias: remove)")
	fmt.Fprintln(w, "  start       Start the daemon")
	fmt.Fprintln(w, "  stop        Stop the daemon")
	fmt.Fprintln(w, "  restart     Restart the daemon")
	fmt.Fprintln(w, "  status      Print supervisor-reported status")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Supervisor: launchd on macOS, systemd-user on Linux/WSL2, Windows Service Manager on Windows.")
}

// serviceStopTimeout bounds how long Stop waits for the directory
// goroutine to drain before giving up (the supervisor will then kill us).
const serviceStopTimeout = 5 * time.Second

// directoryService implements service.Interface. The supervisor calls
// Start asynchronously; Start launches the directory core in a goroutine
// and returns. Stop cancels the serve context and waits for the goroutine
// to drain — this works on every platform, including Windows SCM where no
// stop signal is ever delivered to the process.
type directoryService struct {
	addr   string
	logger service.Logger
	cancel context.CancelFunc
	exit   chan struct{}
}

func (s *directoryService) Start(_ service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.exit = make(chan struct{})
	go func() {
		defer close(s.exit)
		// Reuse the directory subcommand core. Supervisor stdout is wired
		// into the platform log (Console.app / journalctl / Event Log).
		code := serveDirectory(ctx, s.addr, os.Stdout)
		if code != 0 && s.logger != nil {
			_ = s.logger.Errorf("directory exited with code %d", code)
		}
	}()
	return nil
}

func (s *directoryService) Stop(_ service.Service) error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.exit == nil {
		return nil
	}
	select {
	case <-s.exit:
		return nil
	case <-time.After(serviceStopTimeout):
		return fmt.Errorf("directory did not shut down within %s", serviceStopTimeout)
	}
}

// buildService composes a service.Service for the given listen address and
// extra service-config knobs. Centralised so install/start/stop/status all
// see the same Name, Arguments and Description.
func buildService(addr string) (service.Service, *directoryService, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locate own executable: %w", err)
	}
	// Best-effort: resolve symlinks so the supervisor unit always points
	// at the real binary, even if the user later moves a symlink.
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	prog := &directoryService{addr: addr}

	cfg := &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplay,
		Description: serviceDescription,
		Executable:  exe,
		Arguments:   []string{"service", "run", "--addr", addr},
		Option:      platformOptions(),
	}

	svc, err := service.New(prog, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build service: %w", err)
	}
	prog.logger, _ = svc.Logger(nil)
	return svc, prog, nil
}

// platformOptions returns the kardianos/service option map appropriate for
// the current OS. We always run as a USER service (no root) — no admin
// prompt on install, and per-user isolation between machines with multiple
// developer accounts.
func platformOptions() service.KeyValue {
	opt := service.KeyValue{}
	switch runtime.GOOS {
	case "darwin":
		opt["UserService"] = true // ~/Library/LaunchAgents
		opt["RunAtLoad"] = true   // start at login
		opt["KeepAlive"] = true   // restart on crash
	case "linux":
		opt["UserService"] = true // ~/.config/systemd/user
		opt["Restart"] = "on-failure"
		opt["LogOutput"] = true
		// systemd will only auto-start at login if `loginctl enable-linger <user>`
		// — we surface this in `a2abridge doctor`.
	case "windows":
		// Windows Service Manager doesn't really do user services; install
		// runs in the user context. Auto-start at boot is opt-out.
		opt["DelayedAutoStart"] = true
		opt["StartType"] = "automatic"
	}
	return opt
}

// svcInstall: install + start. We accept --addr to allow non-default port,
// and --federation to bake an ed25519 cert + key into the service unit so
// the directory daemon comes up with mTLS already wired.
func svcInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultDirectoryAddr, "directory listen address baked into the service unit")
	federation := fs.Bool("federation", false, "generate ed25519 cert+key under ~/.a2abridge/tls and enable mTLS")
	cn := fs.String("cn", "", "common name for the federation cert (default: hostname)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *federation {
		if err := provisionFederation(*cn, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "a2abridge service install --federation: %v\n", err)
			return 1
		}
	}

	svc, _, err := buildService(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "a2abridge service install: %v\n", err)
		return 1
	}

	if err := svc.Install(); err != nil {
		// kardianos/service doesn't expose an "already installed" sentinel;
		// match against the platform-specific error strings as a soft fallback.
		if isAlreadyInstalled(err) {
			fmt.Fprintln(stdout, "service already installed; reinstalling")
			_ = svc.Stop()
			_ = svc.Uninstall()
			if err := svc.Install(); err != nil {
				fmt.Fprintf(stderr, "reinstall failed: %v\n", err)
				return 1
			}
		} else {
			fmt.Fprintf(stderr, "install failed: %v\n", err)
			return 1
		}
	}
	if err := svc.Start(); err != nil {
		fmt.Fprintf(stderr, "start failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "service %q installed and started on %s (%s, user-level)\n",
		serviceName, *addr, supervisorName())
	if runtime.GOOS == "linux" {
		fmt.Fprintln(stdout, "Tip: enable auto-start at boot with: sudo loginctl enable-linger $USER")
	}
	return 0
}

func svcAction(action string, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "a2abridge service %s: unexpected arguments: %s\n", action, strings.Join(args, " "))
		return 2
	}
	svc, _, err := buildService(defaultDirectoryAddr)
	if err != nil {
		fmt.Fprintf(stderr, "a2abridge service %s: %v\n", action, err)
		return 1
	}
	if err := service.Control(svc, action); err != nil {
		fmt.Fprintf(stderr, "%s failed: %v\n", action, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s ok\n", action)
	return 0
}

func svcStatus(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintf(stderr, "a2abridge service status: unexpected arguments: %s\n", strings.Join(args, " "))
		return 2
	}
	svc, _, err := buildService(defaultDirectoryAddr)
	if err != nil {
		fmt.Fprintf(stderr, "a2abridge service status: %v\n", err)
		return 1
	}
	st, err := svc.Status()
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "service: %s\nsupervisor: %s\nstatus: %s\nbinary: %s\nversion: %s\n",
		serviceName, supervisorName(), statusString(st), executablePath(), buildinfo.Get().Version)
	return 0
}

// svcRun is the in-process supervisor entry point. The kardianos/service
// machinery calls Start/Stop on the directoryService; we just block here
// until those signals fire.
func svcRun(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("service run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultDirectoryAddr, "listen address (filled by service unit)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	svc, _, err := buildService(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "service run: %v\n", err)
		return 1
	}

	if err := svc.Run(); err != nil {
		// Run blocks until the supervisor signals stop. Errors here are real.
		slog.New(slog.NewJSONHandler(stderr, nil)).Error("service run", "err", err)
		return 1
	}
	return 0
}

func statusString(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func supervisorName() string {
	switch runtime.GOOS {
	case "darwin":
		return "launchd (user)"
	case "linux":
		return "systemd --user"
	case "windows":
		return "Windows Service Manager"
	default:
		return runtime.GOOS
	}
}

func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// provisionFederation generates an ed25519 cert + key under
// ~/.a2abridge/tls (or refreshes it) and prints the env block the user
// must paste into their IDE config so the bridge picks it up. We don't
// modify ~/.claude/settings.json from here — that's `a2abridge install`'s
// job and re-running it preserves the .bak chain.
func provisionFederation(cn string, stdout, stderr io.Writer) error {
	if cn == "" {
		host, err := os.Hostname()
		if err == nil {
			cn = host
		} else {
			cn = "a2abridge-peer"
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	dir := filepath.Join(home, ".a2abridge", "tls")
	cert, key, err := security.GenerateEd25519Cert(dir, cn)
	if err != nil {
		return fmt.Errorf("generate cert: %w", err)
	}
	fmt.Fprintf(stdout,
		"federation cert generated for %q\n"+
			"  cert: %s\n  key:  %s\n\n"+
			"Add to your IDE's mcpServers.a2a.env so bridges pick up TLS:\n"+
			"  A2A_TLS_CERT=%s\n  A2A_TLS_KEY=%s\n  A2A_TRUST_ROOTS=%s   # extend with peer certs (':' separated)\n  A2A_PEER_ALLOW=%s    # optional CN/SAN allow-list\n\n",
		cn, cert, key, cert, key, cert, cn,
	)
	return nil
}

// isAlreadyInstalled is a heuristic — kardianos/service surfaces the OS
// error as plain text. We match well-known suffixes.
func isAlreadyInstalled(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "already installed") ||
		strings.Contains(msg, "init already exists")
}
