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

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/server"

	"github.com/vbcherepanov/a2abridge/v4/internal/agent"
	"github.com/vbcherepanov/a2abridge/v4/internal/buildinfo"
	"github.com/vbcherepanov/a2abridge/v4/internal/mdns"
	"github.com/vbcherepanov/a2abridge/v4/internal/security"
)

// Bridge timing knobs.
const (
	outgoingPollInterval       = 5 * time.Second
	outgoingPollRequestTimeout = 4 * time.Second
	outgoingMaxAge             = 10 * time.Minute
	shutdownTimeout            = 3 * time.Second

	// readHeaderTimeout and requestReadTimeout bound reading one request.
	// net/http clears the read deadline once the body is consumed, so SSE
	// responses stay open past it.
	readHeaderTimeout  = 10 * time.Second
	requestReadTimeout = time.Minute
)

// providerURL is advertised in the Agent Card's provider block.
const providerURL = "https://github.com/vbcherepanov/a2abridge"

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

	// Bind is also our singleton lock. On a fast parent restart the previous
	// bridge may still be releasing this port for a few hundred milliseconds, so
	// retry a bounded number of times to let the new bridge win once the old one
	// lets go. If the port is STILL held after the grace window, another bridge
	// already serves this agent — defer to it and exit cleanly (0) rather than
	// erroring or running portless, so exactly one bridge exists per agent.
	// Combined with the pdeathsig/stdin-EOF shutdown above, a stale incumbent is
	// already gone and a surviving one is the legitimate owner. Non-EADDRINUSE
	// bind errors are real failures and are not retried.
	var ln net.Listener
	for attempt := 0; ; attempt++ {
		ln, err = net.Listen("tcp", *bindAddr)
		if err == nil {
			break
		}
		if !isAddrInUse(err) {
			log.Error("listen", "err", err)
			return 1
		}
		if attempt >= 10 {
			log.Info("listen: port already held by another bridge, deferring", "addr", *bindAddr)
			return 0
		}
		log.Warn("listen: address in use, retrying", "attempt", attempt+1)
		time.Sleep(300 * time.Millisecond)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	fed := security.FromEnv()
	scheme := "http"
	if fed.Enabled() {
		scheme = "https"
	}
	selfURL := fmt.Sprintf("%s://%s:%d", scheme, *advertiseHost, port)

	store := agent.NewStore()
	store.Log = log
	defer store.Close()
	store.InboxPath = filepath.Join(resolvedStateDir, "inbox.json")
	store.LoadInbox() // durable inbox: restore messages that arrived (and weren't drained) before a restart
	executor := agent.NewExecutor(store, log)
	pushConfigs := push.NewInMemoryStore()
	tasks := agent.NewTTLStore(pushConfigs, log)
	defer tasks.Close()
	cwd, err := os.Getwd()
	if err != nil {
		log.Warn("working directory unknown", "err", err)
	}

	responderMode := os.Getenv("A2A_RESPONDER")
	nudgeMode := os.Getenv("A2A_NUDGE")

	skillList := splitCSV(*skills)
	agentSkills := make([]a2a.AgentSkill, 0, len(skillList))
	for _, sk := range skillList {
		agentSkills = append(agentSkills, a2a.AgentSkill{
			ID:          sk,
			Name:        sk,
			Description: "agent skill: " + sk,
			Tags:        []string{sk},
		})
	}
	card := &a2a.AgentCard{
		Name:        *name,
		Description: fmt.Sprintf("a2abridge agent (%s) at %s", *name, cwd),
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(selfURL, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(selfURL, a2a.TransportProtocolHTTPJSON),
		},
		Version:            buildinfo.Version,
		Capabilities:       a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:  []string{agent.TextMediaType},
		DefaultOutputModes: []string{agent.TextMediaType},
		Skills:             agentSkills,
		Provider:           &a2a.AgentProvider{Org: "a2abridge", URL: providerURL},
	}
	if *model != "" {
		card.Description = card.Description + " | model=" + *model
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Die with our parent: if the MCP host (e.g. claude) exits, this bridge must
	// shut down and release its port rather than orphaning to init (ppid 1) and
	// squatting the port so the next bridge cannot bind ("address already in
	// use"), which silently kills the agent's outbound send path. On Linux this
	// arms PR_SET_PDEATHSIG(SIGTERM), delivered by the kernel the moment the
	// parent dies and routed into the handler above; on other platforms it is a
	// no-op and we rely on the stdin-EOF shutdown below (ServeStdio returning).
	setParentDeathSignal(log)

	if responderMode != "" {
		r, rerr := agent.NewResponder(responderMode, card, executor, log)
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
		switch nudgeMode {
		case "":
			log.Warn("A2A_NUDGE=auto but no backend detected (not in tmux, not on darwin)")
		case "dtach":
			// dtach backend: inject into the supervising dtach master socket.
			// Socket via A2A_NUDGE_SOCKET, defaulting to ~/.dtach/<name>.
			socket := os.Getenv("A2A_NUDGE_SOCKET")
			if socket == "" {
				socket = filepath.Join(os.Getenv("HOME"), ".dtach", *name)
			}
			n := agent.NewDtachNudger(socket, log)
			store.OnIncoming = n.Handle
			log.Info("dtach nudger enabled", "socket", socket)
		default:
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

	// Outbound peer calls and webhook deliveries present the same client
	// cert and trust roots as the server; nil in plain loopback mode.
	clientTLS, err := fed.ClientTLSConfig()
	if err != nil {
		log.Error("client tls config", "err", err)
		return 1
	}
	peers := agent.NewPeers(clientTLS, log)
	// The SSRF guard matters once the bridge is reachable across machines; a
	// loopback bridge's webhooks legitimately point at 127.0.0.1.
	allowPrivatePush := !fed.Enabled() || os.Getenv("A2A_PUSH_ALLOW_PRIVATE") == "1"
	pushSender := agent.NewPushSender(clientTLS, allowPrivatePush, log)

	a2aHandler, err := agent.NewA2AHTTPHandler(card, executor, tasks, pushConfigs, pushSender, log)
	if err != nil {
		log.Error("a2a handler", "err", err)
		return 1
	}
	httpSrv := &http.Server{
		Handler:           a2aHandler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       requestReadTimeout,
	}
	if fed.Enabled() {
		tlsCfg, terr := fed.ServerTLSConfig()
		if terr != nil {
			log.Error("tls config", "err", terr)
			return 1
		}
		httpSrv.TLSConfig = tlsCfg
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
		t := time.NewTicker(outgoingPollInterval)
		defer t.Stop()
		fetcher := func(peerURL, taskID string) (*a2a.Task, error) {
			fctx, cancel := context.WithTimeout(ctx, outgoingPollRequestTimeout)
			defer cancel()
			return peers.GetTask(fctx, peerURL, a2a.TaskID(taskID))
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := store.PollOutgoing(fetcher, outgoingMaxAge); n > 0 {
					log.Info("outgoing replies injected", "count", n)
				}
			}
		}
	}()

	mcpSrv := server.NewMCPServer("a2abridge", buildinfo.Version)
	agent.RegisterTools(mcpSrv, &agent.MCPDeps{
		Lifetime:     ctx,
		Store:        store,
		Executor:     executor,
		Peers:        peers,
		OwnCard:      card,
		SelfURL:      selfURL,
		DirectoryURL: *directoryURL,
		Log:          log,
	})

	go func() {
		log.Info("mcp stdio server starting")
		// ServeStdio reads os.Stdin and returns when it closes — which happens
		// when the parent (MCP host) goes away. Always shut the bridge down when
		// it returns, even on a clean EOF (err == nil), so the bridge never
		// outlives its parent while still holding the port.
		if err := server.ServeStdio(mcpSrv); err != nil {
			log.Error("mcp serve", "err", err)
		}
		log.Info("mcp stdio server exited, shutting down bridge")
		stop()
	}()

	<-ctx.Done()
	log.Info("shutting down")
	// Do NOT delete store.InboxPath on shutdown — the snapshot must survive a
	// bounce so LoadInbox() can restore undrained messages (durable delivery).
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	// Executions run detached from HTTP requests, so events may still be
	// queued for webhooks; give them their own drain budget.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer drainCancel()
	if err := pushSender.Close(drainCtx); err != nil {
		log.Warn("push notifications not fully drained", "err", err)
	}
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
		return explicit, ensureStateGitignore(explicit)
	}
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, ".a2a")
		if writable(cwd) {
			if err := os.MkdirAll(candidate, 0o755); err == nil {
				return candidate, ensureStateGitignore(candidate)
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
	return fallback, ensureStateGitignore(fallback)
}

// ensureStateGitignore drops a `*` .gitignore into the state dir so logs
// and inbox snapshots never get committed when the dir lives inside a
// repository. An existing .gitignore is left untouched.
func ensureStateGitignore(dir string) error {
	p := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte("*\n"), 0o600)
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
