package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"

	"github.com/vbcherepanov/a2abridge/internal/a2a"
	"github.com/vbcherepanov/a2abridge/internal/agent"
	"github.com/vbcherepanov/a2abridge/internal/buildinfo"
	"github.com/vbcherepanov/a2abridge/internal/mdns"
	"github.com/vbcherepanov/a2abridge/internal/security"
)

// RunBridge runs the per-agent bridge: A2A HTTP server + MCP stdio server.
func RunBridge(args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	directoryURL := fs.String("directory", envOr("A2A_DIRECTORY", "http://127.0.0.1:7777"), "directory service base URL")
	bindAddr := fs.String("bind", envOr("A2A_BIND", "127.0.0.1:0"), "HTTP bind address (port 0 = random)")
	advertiseHost := fs.String("advertise-host", envOr("A2A_ADVERTISE_HOST", "127.0.0.1"), "hostname peers will use to reach this agent")
	name := fs.String("name", envOr("A2A_NAME", autoName()), "agent display name")
	model := fs.String("model", envOr("A2A_MODEL", ""), "model identifier (claude-opus-4-7, gpt-5, ...)")
	skills := fs.String("skills", envOr("A2A_SKILLS", ""), "comma-separated skills")
	idFlag := fs.String("id", envOr("A2A_ID", ""), "stable agent id (default: random)")
	stateDir := fs.String("state-dir", envOr("A2A_STATE_DIR", ""), "per-bridge state directory (default: ./.a2a or ~/.a2abridge/state/<pid>)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge bridge [flags]\n\nRun the MCP stdio server that wraps this agent into an A2A peer.\nNormally invoked by your IDE — do not run manually.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *idFlag == "" {
		*idFlag = uuid.NewString()
	}

	resolvedStateDir, err := resolveStateDir(*stateDir)
	if err != nil {
		fmt.Fprintf(stderr, "a2abridge bridge: state dir: %v\n", err)
		return 1
	}

	logFile, _ := os.OpenFile(filepath.Join(resolvedStateDir, "bridge.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	var h slog.Handler
	if logFile != nil {
		h = slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		h = slog.NewJSONHandler(stderr, nil)
	}
	log := slog.New(h).With("agent", *name, "id", *idFlag, "state_dir", resolvedStateDir)

	ln, err := net.Listen("tcp", *bindAddr)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	port := ln.Addr().(*net.TCPAddr).Port

	fed := security.FromEnv()
	scheme := "http"
	if fed.Enabled() {
		scheme = "https"
	}
	selfURL := fmt.Sprintf("%s://%s:%d", scheme, *advertiseHost, port)

	store := agent.NewStore()
	store.InboxPath = filepath.Join(resolvedStateDir, fmt.Sprintf("inbox-%d.json", os.Getppid()))
	cwd, _ := os.Getwd()

	responderMode := os.Getenv("A2A_RESPONDER")
	nudgeMode := os.Getenv("A2A_NUDGE")

	skillList := splitCSV(*skills)
	agentSkills := make([]a2a.AgentSkill, 0, len(skillList))
	for _, sk := range skillList {
		agentSkills = append(agentSkills, a2a.AgentSkill{
			ID:          sk,
			Name:        sk,
			Description: "agent skill: " + sk,
		})
	}
	card := a2a.AgentCard{
		ProtocolVersion:    a2a.ProtocolVersion,
		Name:               *name,
		Description:        fmt.Sprintf("a2abridge agent (%s) at %s", *name, cwd),
		URL:                selfURL,
		PreferredTransport: "JSONRPC",
		Version:            buildinfo.Version,
		Capabilities:       a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             agentSkills,
		Provider:           &a2a.AgentProvider{Organization: "a2abridge"},
	}
	if *model != "" {
		card.Description = card.Description + " | model=" + *model
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if responderMode != "" {
		r, rerr := agent.NewResponder(responderMode, card, store, log)
		if rerr != nil {
			log.Error("responder init", "err", rerr)
		} else {
			store.OnIncoming = r.Handle
			defer r.Close()
			log.Info("autonomous responder enabled", "mode", responderMode)
		}
	} else if nudgeMode != "" {
		if nudgeMode == "auto" {
			nudgeMode = agent.DetectNudgeMode()
		}
		if nudgeMode == "" {
			log.Warn("A2A_NUDGE=auto but no backend detected (not in tmux, not on darwin)")
		} else {
			tty := parentTTY(os.Getppid())
			if tty == "" {
				log.Warn("nudge requested but parent TTY unknown, disabled")
			} else {
				n := agent.NewNudger(nudgeMode, tty, log)
				store.OnIncoming = n.Handle
				log.Info("tty nudger enabled", "mode", nudgeMode, "tty", tty)
			}
		}
	}

	a2aSrv := &a2a.Server{Card: card, Handler: store, Log: log}
	httpSrv := &http.Server{
		Handler:           a2aSrv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if fed.Enabled() {
		tlsCfg, terr := fed.ServerTLSConfig()
		if terr != nil {
			log.Error("tls config", "err", terr)
			return 1
		}
		httpSrv.TLSConfig = tlsCfg
		// Outbound clients (peers, directory heartbeat, SSE subscribers)
		// inherit the same trust roots + client cert through this default
		// transport.
		if clientCfg, cerr := fed.ClientTLSConfig(); cerr == nil && clientCfg != nil {
			a2a.DefaultTransport = &http.Transport{TLSClientConfig: clientCfg}
		}
	}

	go func() {
		log.Info("a2a server listening", "url", selfURL, "tls", fed.Enabled())
		var serveErr error
		if fed.Enabled() {
			serveErr = httpSrv.ServeTLS(ln, "", "")
		} else {
			serveErr = httpSrv.Serve(ln)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Error("serve", "err", serveErr)
			stop()
		}
	}()

	go agent.Heartbeat(ctx, *directoryURL, selfURL)

	// LAN discovery via mDNS — opt-in. Useful for cross-machine setups
	// without a shared directory daemon.
	if os.Getenv("A2A_MDNS") == "1" {
		if pub, perr := mdns.Publish(*name, selfURL, log); perr != nil {
			log.Warn("mdns publish failed", "err", perr)
		} else {
			defer pub.Close()
			log.Info("mdns publishing enabled", "instance", *name)
		}
	}

	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		fetcher := func(peerURL, taskID string) (*a2a.Task, error) {
			c := a2a.NewClient(peerURL)
			fctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			return c.GetTask(fctx, taskID)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := store.PollOutgoing(fetcher, 10*time.Minute); n > 0 {
					log.Info("outgoing replies injected", "count", n)
				}
			}
		}
	}()

	mcpSrv := server.NewMCPServer("a2abridge", buildinfo.Version)
	agent.RegisterTools(mcpSrv, &agent.MCPDeps{
		Store:        store,
		OwnCard:      card,
		DirectoryURL: *directoryURL,
	})

	go func() {
		log.Info("mcp stdio server starting")
		if err := server.ServeStdio(mcpSrv); err != nil {
			log.Error("mcp serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	_ = os.Remove(store.InboxPath)
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	return 0
}

// resolveStateDir picks the per-bridge state directory:
//  1. explicit --state-dir flag wins;
//  2. cwd/.a2a if cwd is writable;
//  3. ~/.a2abridge/state/<ppid> as a fallback.
//
// The directory is created with 0o755 and a guard against
// pointing at a non-directory.
func resolveStateDir(explicit string) (string, error) {
	if explicit != "" {
		if err := os.MkdirAll(explicit, 0o755); err != nil {
			return "", err
		}
		return explicit, nil
	}
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, ".a2a")
		if writable(cwd) {
			if err := os.MkdirAll(candidate, 0o755); err == nil {
				return candidate, nil
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	fallback := filepath.Join(home, ".a2abridge", "state", fmt.Sprint(os.Getppid()))
	if err := os.MkdirAll(fallback, 0o755); err != nil {
		return "", err
	}
	return fallback, nil
}

func writable(dir string) bool {
	probe, err := os.CreateTemp(dir, ".a2a-write-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return true
}

// autoName derives a human-readable agent name from the parent process.
// Examples: "claude-ttys003", "claude-pid41547", "codex-ttys001". On Windows
// where ps is unavailable, falls back to "agent-pid<n>".
func autoName() string {
	ppid := os.Getppid()
	parent := parentCommand(ppid)
	tty := parentTTY(ppid)

	var base string
	switch {
	case strings.Contains(parent, "claude"):
		base = "claude"
	case strings.Contains(parent, "codex"):
		base = "codex"
	case strings.Contains(parent, "cursor"):
		base = "cursor"
	case strings.Contains(parent, "code"): // VS Code (Cline, Continue)
		base = "vscode"
	case parent != "":
		base = parent
	default:
		base = "agent"
	}

	suffix := tty
	if suffix == "" || suffix == "??" {
		suffix = fmt.Sprintf("pid%d", ppid)
	} else {
		suffix = strings.TrimPrefix(suffix, "/dev/")
	}
	return base + "-" + suffix
}

// parentCommand returns the basename of the parent process command. Returns
// "" on platforms where ps is not available (e.g. native Windows).
func parentCommand(ppid int) string {
	if _, err := exec.LookPath("ps"); err != nil {
		return ""
	}
	out, err := exec.Command("ps", "-o", "comm=", "-p", fmt.Sprint(ppid)).Output()
	if err != nil {
		return ""
	}
	comm := strings.TrimSpace(string(out))
	if idx := strings.LastIndex(comm, "/"); idx >= 0 {
		comm = comm[idx+1:]
	}
	return comm
}

func parentTTY(ppid int) string {
	if _, err := exec.LookPath("ps"); err != nil {
		return ""
	}
	out, err := exec.Command("ps", "-o", "tty=", "-p", fmt.Sprint(ppid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
