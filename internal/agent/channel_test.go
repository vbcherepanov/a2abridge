package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/vbcherepanov/a2abridge/v4/internal/security"
)

// stdioPeer drives an MCP server over the real stdio transport, the way
// Claude Code does: JSON lines in, JSON lines out.
type stdioPeer struct {
	in    *io.PipeWriter
	lines chan map[string]any
}

// startStdio serves srv over in-memory pipes with the given stdio options.
// mcp-go v1.6.0 keeps one global stdio session: its initialized flag and its
// notification queue carry over from one test to the next. Tests using it
// must not run in parallel, and none may rely on the session being
// uninitialized (Channel keeps its own ready flag for that).
func startStdio(t *testing.T, srv *server.MCPServer, opts ...server.StdioOption) *stdioPeer {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := server.NewStdioServer(srv)
	for _, opt := range opts {
		opt(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Listen(ctx, inR, outW)
		_ = outW.Close()
	}()
	p := &stdioPeer{in: inW, lines: make(chan map[string]any, 64)}
	go func() {
		defer close(p.lines)
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				p.lines <- m
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		_ = outR.Close()
		<-done
	})
	return p
}

func (p *stdioPeer) send(t *testing.T, msg string) {
	t.Helper()
	if _, err := io.WriteString(p.in, msg+"\n"); err != nil {
		t.Fatalf("write to stdio server: %v", err)
	}
}

// next returns the first line matching match, skipping the others.
func (p *stdioPeer) next(t *testing.T, what string, match func(map[string]any) bool) map[string]any {
	t.Helper()
	timeout := time.After(waitTimeout)
	for {
		select {
		case m, ok := <-p.lines:
			if !ok {
				t.Fatalf("stdio server closed before %s", what)
			}
			if match(m) {
				return m
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (p *stdioPeer) response(t *testing.T, id string) map[string]any {
	t.Helper()
	return p.next(t, "response "+id, func(m map[string]any) bool { return m["id"] == id })
}

func (p *stdioPeer) channelEvent(t *testing.T) map[string]any {
	t.Helper()
	m := p.next(t, "a claude/channel notification", func(m map[string]any) bool {
		return m["method"] == "notifications/claude/channel"
	})
	params, _ := m["params"].(map[string]any)
	return params
}

// initialize runs the legacy handshake as Claude Code does after its
// server/discover probe, and returns the initialize result.
func (p *stdioPeer) initialize(t *testing.T, clientName, version string) map[string]any {
	t.Helper()
	p.send(t, `{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"`+clientName+`","version":"2.1.332"}}}`)
	res, _ := p.response(t, "init")["result"].(map[string]any)
	p.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return res
}

// discoverProbe is the server/discover request Claude Code 2.1.332 sends
// first, copied from a captured session.
const discoverProbe = `{"jsonrpc":"2.0","id":"discover","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"2.1.332"},"io.modelcontextprotocol/clientCapabilities":{"roots":{"listChanged":true}}}}}`

func discoveredVersions(t *testing.T, p *stdioPeer) []string {
	t.Helper()
	p.send(t, discoverProbe)
	res, _ := p.response(t, "discover")["result"].(map[string]any)
	raw, _ := res["supportedVersions"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// warnings returns the warn-level lines logged so far.
func (s *syncBuffer) warnings() []string {
	var out []string
	for _, line := range strings.Split(s.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			out = append(out, line)
		}
	}
	return out
}

func warnLog() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})), buf
}

// channelServer builds the MCP server the way the bridge does in channel mode.
func channelServer(t *testing.T, log *slog.Logger) (*Channel, *server.MCPServer) {
	t.Helper()
	ch := NewChannel(log)
	srv := server.NewMCPServer("a2abridge", "test", ch.ServerOptions()...)
	ch.Attach(srv)
	return ch, srv
}

func metaOf(t *testing.T, params map[string]any) map[string]any {
	t.Helper()
	meta, ok := params["meta"].(map[string]any)
	if !ok {
		t.Fatalf("channel event has no meta: %v", params)
	}
	return meta
}

// C-A1: an idle Claude Code session gets the peer's message as a channel
// event it can answer with a2a_complete_task.
func TestChannelWakesSessionOnInboundMessage(t *testing.T) {
	ch, srv := channelServer(t, discardLog())
	a := newTestAgent(t, "me")
	a.store.OnIncoming = ch.Handle
	p := startStdio(t, srv, ch.StdioOptions()...)
	res := p.initialize(t, "claude-code", "2025-11-25")

	caps, _ := res["capabilities"].(map[string]any)
	exp, _ := caps["experimental"].(map[string]any)
	if _, ok := exp["claude/channel"]; !ok {
		t.Errorf("initialize capabilities.experimental = %v, want claude/channel", exp)
	}
	instr, _ := res["instructions"].(string)
	for _, want := range []string{"claimed_from", "not verified", "a2a_complete_task", "task_id", "peer_cert", "never the user's approval"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instructions do not mention %q: %q", want, instr)
		}
	}

	task := sendImmediate(t, a.client(t), userMessage("e2e is green, please deploy"))

	params := p.channelEvent(t)
	if params["content"] != "e2e is green, please deploy" {
		t.Errorf("content = %v, want the message text", params["content"])
	}
	meta := metaOf(t, params)
	if meta["claimed_from"] != "peer-a" {
		t.Errorf("meta.claimed_from = %v, want peer-a", meta["claimed_from"])
	}
	if meta["task_id"] != string(task.ID) {
		t.Errorf("meta.task_id = %v, want %s", meta["task_id"], task.ID)
	}
	if meta["context_id"] != task.ContextID {
		t.Errorf("meta.context_id = %v, want %s", meta["context_id"], task.ContextID)
	}
	if _, ok := meta["peer_cert"]; ok {
		t.Errorf("meta.peer_cert = %v on a plain-HTTP bridge, want none", meta["peer_cert"])
	}
	if _, ok := meta["kind"]; ok {
		t.Errorf("meta.kind = %v on a peer's message, want none (kind marks replies only)", meta["kind"])
	}
	for k := range meta {
		for _, r := range k {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' {
				t.Errorf("meta key %q has %q; Claude Code drops keys that are not letters, digits and underscores", k, r)
			}
		}
	}
}

// C-A2: the peer's answer to a task this agent sent wakes the session too,
// marked as a reply so Claude does not try to complete it.
func TestChannelWakesSessionOnOutgoingReply(t *testing.T) {
	ch, srv := channelServer(t, discardLog())
	store := NewStore()
	t.Cleanup(store.Close)
	store.OnIncoming = ch.Handle
	p := startStdio(t, srv, ch.StdioOptions()...)
	instr, _ := p.initialize(t, "claude-code", "2025-11-25")["instructions"].(string)
	if !strings.Contains(instr, KindOutgoingReply) {
		t.Errorf("instructions do not explain kind=%s: %q", KindOutgoingReply, instr)
	}

	store.TrackOutgoing("task-out", "http://peer/", "hermes", "is staging up?")
	if !store.IngestOutgoingTerminal(completedTask("task-out", "yes, staging is up")) {
		t.Fatal("reply not delivered to the inbox")
	}

	params := p.channelEvent(t)
	if content, _ := params["content"].(string); !strings.Contains(content, "yes, staging is up") {
		t.Errorf("content = %q, want the reply text", content)
	}
	meta := metaOf(t, params)
	if meta["kind"] != KindOutgoingReply {
		t.Errorf("meta.kind = %v, want %s", meta["kind"], KindOutgoingReply)
	}
	if meta["task_id"] != "task-out" || meta["claimed_from"] != "hermes" {
		t.Errorf("meta = %v, want task_id task-out and claimed_from hermes", meta)
	}
}

// newMTLSTestAgent is newTestAgent served over federation mTLS, with a
// client that presents a certificate named clientCN.
func newMTLSTestAgent(t *testing.T, clientCN string) (*testAgent, *Peers) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, err := security.GenerateEd25519Cert(dir, clientCN)
	if err != nil {
		t.Fatal(err)
	}
	fed := security.FederationConfig{CertFile: certPath, KeyFile: keyPath, TrustRoots: []string{certPath}}
	serverTLS, err := fed.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, err := fed.ClientTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	log := discardLog()
	store := NewStore()
	t.Cleanup(store.Close)
	pushConfigs := push.NewInMemoryStore()
	tasks := NewTTLStore(pushConfigs, log)
	t.Cleanup(tasks.Close)
	executor := NewExecutor(store, log)
	srv := httptest.NewUnstartedServer(nil)
	url := "https://" + srv.Listener.Addr().String()
	card := &a2a.AgentCard{
		Name: "me", Description: "test agent", Version: "test",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:   []string{TextMediaType},
		DefaultOutputModes:  []string{TextMediaType},
		Skills:              []a2a.AgentSkill{},
	}
	sender := NewPushSender(clientTLS, true, log)
	t.Cleanup(func() { closePushSender(t, sender) })
	handler, err := NewA2AHTTPHandler(card, executor, tasks, pushConfigs, sender, log)
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = handler
	srv.TLS = serverTLS
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &testAgent{store: store, executor: executor, tasks: tasks, pushConfigs: pushConfigs, card: card, url: url},
		NewPeers(clientTLS, log)
}

// C-A3: with federation TLS on, the event carries the verified certificate
// name next to the sender's own claim.
func TestChannelEventCarriesPeerCertificateName(t *testing.T) {
	ch, srv := channelServer(t, discardLog())
	a, peers := newMTLSTestAgent(t, "laptop-a")
	a.store.OnIncoming = ch.Handle
	p := startStdio(t, srv, ch.StdioOptions()...)
	p.initialize(t, "claude-code", "2025-11-25")

	c, err := peers.Client(context.Background(), a.url)
	if err != nil {
		t.Fatalf("connect over mTLS: %v", err)
	}
	t.Cleanup(func() { peers.Release(c) })
	sendImmediate(t, c, userMessage("hello over mTLS"))

	meta := metaOf(t, p.channelEvent(t))
	if meta["peer_cert"] != "laptop-a" {
		t.Errorf("meta.peer_cert = %v, want laptop-a", meta["peer_cert"])
	}
	if meta["claimed_from"] != "peer-a" {
		t.Errorf("meta.claimed_from = %v, want peer-a (the claim stays next to the certificate)", meta["claimed_from"])
	}
}

// C-A4: auto never selects channel mode, inside tmux or not.
func TestDetectNudgeModeNeverPicksChannel(t *testing.T) {
	for _, tmux := range []string{"", "/tmp/tmux-501/default,1,0"} {
		t.Setenv("TMUX", tmux)
		if got := DetectNudgeMode(); got == "channel" {
			t.Errorf("DetectNudgeMode() with TMUX=%q = channel; channel must be set explicitly", tmux)
		}
	}
}

// C-A5: in channel mode the stdio server offers only the legacy protocol
// versions, so Claude Code falls back to initialize, which keeps the
// notification path Claude Code needs for channels.
func TestChannelStdioOffersOnlyLegacyVersions(t *testing.T) {
	ch, srv := channelServer(t, discardLog())
	p := startStdio(t, srv, ch.StdioOptions()...)

	versions := discoveredVersions(t, p)
	if len(versions) == 0 {
		t.Fatal("server/discover returned no supportedVersions")
	}
	for _, v := range versions {
		if mcp.IsModernProtocol(v) {
			t.Errorf("channel mode advertises modern version %s; Claude Code then skips channel events", v)
		}
	}
	if !slices.Contains(versions, mcp.ProtocolVersion20251125) {
		t.Errorf("supportedVersions = %v, want %s", versions, mcp.ProtocolVersion20251125)
	}
	if got := p.initialize(t, "claude-code", "2026-07-28")["protocolVersion"]; got != mcp.ProtocolVersion20251125 {
		t.Errorf("initialize asking for 2026-07-28 negotiated %v, want %s", got, mcp.ProtocolVersion20251125)
	}
}

// C-A5, the other half: every other mode keeps mcp-go's defaults, including
// the modern revision. If an mcp-go upgrade changes the defaults, this fails.
func TestDefaultStdioKeepsModernVersion(t *testing.T) {
	var ch *Channel // any mode other than channel
	srv := server.NewMCPServer("a2abridge", "test", ch.ServerOptions()...)
	p := startStdio(t, srv, ch.StdioOptions()...)
	versions := discoveredVersions(t, p)
	if !slices.Contains(versions, mcp.ProtocolVersion20260728) {
		t.Errorf("supportedVersions outside channel mode = %v, want mcp-go's default including %s", versions, mcp.ProtocolVersion20260728)
	}
}

// C-A7: outside channel mode the server declares no channel and sets no
// channel instructions.
func TestNoChannelCapabilityOutsideChannelMode(t *testing.T) {
	var ch *Channel
	srv := server.NewMCPServer("a2abridge", "test", ch.ServerOptions()...)
	ch.Attach(srv)
	ch.Handle(&InboxEntry{TaskID: "t", Text: "x"}) // must be a no-op
	p := startStdio(t, srv, ch.StdioOptions()...)
	res := p.initialize(t, "claude-code", "2025-11-25")
	caps, _ := res["capabilities"].(map[string]any)
	if exp, ok := caps["experimental"].(map[string]any); ok {
		if _, has := exp["claude/channel"]; has {
			t.Errorf("claude/channel declared outside channel mode")
		}
	}
	if instr, _ := res["instructions"].(string); instr != "" {
		t.Errorf("instructions = %q outside channel mode, want none", instr)
	}
}

// C-A6a: a client that is not Claude Code cannot take channel events; say
// so once.
func TestChannelWarnsOnceForNonClaudeClient(t *testing.T) {
	log, buf := warnLog()
	ch, srv := channelServer(t, log)
	p := startStdio(t, srv, ch.StdioOptions()...)
	p.initialize(t, "cursor", "2025-11-25")
	p.initialize(t, "cursor", "2025-11-25")

	w := buf.warnings()
	if len(w) != 1 {
		t.Fatalf("warnings = %q, want exactly one", w)
	}
	if !strings.Contains(w[0], "cursor") || !strings.Contains(w[0], "Claude Code") {
		t.Errorf("warning %q should name the client and say channels need Claude Code", w[0])
	}
}

func TestChannelNoConnectWarningForClaudeCode(t *testing.T) {
	log, buf := warnLog()
	ch, srv := channelServer(t, log)
	p := startStdio(t, srv, ch.StdioOptions()...)
	discoveredVersions(t, p) // the probe is modern by design and must not warn
	p.initialize(t, "claude-code", "2025-11-25")
	p.send(t, `{"jsonrpc":"2.0","id":"tl","method":"tools/list"}`)
	p.response(t, "tl")
	if w := buf.warnings(); len(w) != 0 {
		t.Errorf("warnings for a Claude Code session on the legacy protocol = %q, want none", w)
	}
}

// C-A6a: a client that sends requests on the modern revision gets no channel
// events; say so once.
func TestChannelWarnsOnceForModernProtocolRequests(t *testing.T) {
	log, buf := warnLog()
	ch, srv := channelServer(t, log)
	srv.AddTool(mcp.NewTool("noop"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	p := startStdio(t, srv, ch.StdioOptions()...)
	modern := `{"jsonrpc":"2.0","id":"%s","method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"9"},"io.modelcontextprotocol/clientCapabilities":{}}}}`
	p.send(t, strings.Replace(modern, "%s", "m1", 1))
	if r := p.response(t, "m1"); r["error"] != nil {
		t.Fatalf("modern tools/list failed: %v", r["error"])
	}
	p.send(t, strings.Replace(modern, "%s", "m2", 1))
	p.response(t, "m2")

	w := buf.warnings()
	if len(w) != 1 {
		t.Fatalf("warnings = %q, want exactly one", w)
	}
	if !strings.Contains(w[0], "2026-07-28") {
		t.Errorf("warning %q should name the modern revision", w[0])
	}
}

// C-A6b: Claude Code does not acknowledge channel events, so on the first
// event the bridge says once what it needs.
func TestChannelWarnsOnceOnFirstEvent(t *testing.T) {
	log, buf := warnLog()
	ch, srv := channelServer(t, log)
	p := startStdio(t, srv, ch.StdioOptions()...)
	p.initialize(t, "claude-code", "2025-11-25")

	ch.Handle(&InboxEntry{TaskID: "t1", Text: "one", From: "peer-a"})
	p.channelEvent(t)
	ch.Handle(&InboxEntry{TaskID: "t2", Text: "two", From: "peer-a"})
	p.channelEvent(t)

	w := buf.warnings()
	if len(w) != 1 {
		t.Fatalf("warnings = %q, want exactly one", w)
	}
	for _, want := range []string{".mcp.json", "--dangerously-load-development-channels"} {
		if !strings.Contains(w[0], want) {
			t.Errorf("first-event warning %q should mention %s", w[0], want)
		}
	}
}

// noChannelEvent fails if a channel notification shows up within d.
func (p *stdioPeer) noChannelEvent(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case m, ok := <-p.lines:
			if !ok {
				return
			}
			if m["method"] == channelMethod {
				t.Fatalf("channel event sent before any client initialized: %v", m)
			}
		case <-deadline:
			return
		}
	}
}

// infoLog captures info-level and above.
func infoLog() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})), buf
}

// Before any client has run initialize, mcp-go has no session to send to.
// The bridge must say the event was not sent (the message waits in the
// inbox for the prompt hook) instead of logging it as sent, and must not use
// up the one-time delivery warning on it.
func TestChannelHandleBeforeInitializeIsNotSent(t *testing.T) {
	log, buf := infoLog()
	ch := NewChannel(log)
	ch.Handle(&InboxEntry{TaskID: "t0", Text: "before attach"}) // no server yet: no panic

	srv := server.NewMCPServer("a2abridge", "test", ch.ServerOptions()...)
	ch.Attach(srv)
	p := startStdio(t, srv, ch.StdioOptions()...)
	ch.Handle(&InboxEntry{TaskID: "t1", Text: "before initialize"})
	p.noChannelEvent(t, 300*time.Millisecond)

	out := buf.String()
	if strings.Contains(out, "channel event queued") {
		t.Errorf("log claims an event was queued before initialize:\n%s", out)
	}
	if !strings.Contains(out, "channel event not sent") || !strings.Contains(out, "inbox") {
		t.Errorf("log should say the event was not sent and the message waits in the inbox:\n%s", out)
	}
	if len(buf.warnings()) != 0 {
		t.Errorf("warnings before any event went out = %q, want none", buf.warnings())
	}

	p.initialize(t, "claude-code", "2025-11-25")
	ch.Handle(&InboxEntry{TaskID: "t2", Text: "after initialize"})
	if got := p.channelEvent(t)["content"]; got != "after initialize" {
		t.Errorf("first event after initialize = %v, want the new message", got)
	}
}

// PeerCert feeds the channel event only; it never reaches the inbox file.
func TestPeerCertIsNotPersisted(t *testing.T) {
	b, err := json.Marshal(InboxEntry{TaskID: "t", PeerCert: "laptop-a"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "laptop-a") {
		t.Errorf("InboxEntry JSON = %s, want no peer certificate name", b)
	}
}

func TestCertName(t *testing.T) {
	cases := []struct {
		name string
		cert *x509.Certificate
		want string
	}{
		{"CN wins over SANs", &x509.Certificate{Subject: pkix.Name{CommonName: "laptop-a"}, DNSNames: []string{"a.example"}, IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}, "laptop-a"},
		{"first DNS SAN without CN", &x509.Certificate{DNSNames: []string{"a.example", "b.example"}, IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}, "a.example"},
		{"IP SAN only", &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("10.0.0.1")}}, "10.0.0.1"},
		{"nothing to name", &x509.Certificate{}, ""},
	}
	for _, tc := range cases {
		if got := certName(tc.cert); got != tc.want {
			t.Errorf("%s: certName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// With a real CA chain the client sends leaf then issuer; peer_cert must
// name the leaf, never the CA every peer shares.
func TestPeerCertNamesTheLeafOfAChain(t *testing.T) {
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: "laptop-a"}}
	ca := &x509.Certificate{Subject: pkix.Name{CommonName: "Fleet CA"}}
	a := newTestAgentWithServer(t, "me", func(s *http.Server) {
		next := s.Handler
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, ca}}
			next.ServeHTTP(w, r)
		})
	})
	task := sendImmediate(t, a.client(t), userMessage("hi"))
	if got := a.inboxEntryFor(t, task.ID).PeerCert; got != "laptop-a" {
		t.Errorf("PeerCert = %q, want the leaf's name laptop-a", got)
	}
}

// When the client stops reading, mcp-go's notification queue fills up. The
// bridge must say which message was not sent, not log it as queued.
func TestChannelReportsEventsTheClientDoesNotTake(t *testing.T) {
	log, buf := warnLog()
	ch, srv := channelServer(t, log)
	p := startStdio(t, srv, ch.StdioOptions()...)
	p.initialize(t, "claude-code", "2025-11-25")

	// Nothing reads p.lines from here: its buffer, the pipe and then
	// mcp-go's 100-slot queue fill up.
	for i := range 400 {
		ch.Handle(&InboxEntry{TaskID: fmt.Sprintf("t%d", i), Text: "flood"})
	}
	waitFor(t, "a not-sent warning", func() bool {
		return strings.Contains(buf.String(), "channel event not sent")
	})
	if !strings.Contains(buf.String(), "task=t") {
		t.Errorf("not-sent warning should name the task:\n%s", buf.String())
	}

	// Drain, so the shared mcp-go stdio session starts the next test empty.
	for quiet := time.After(500 * time.Millisecond); ; {
		select {
		case _, ok := <-p.lines:
			if !ok {
				return
			}
			quiet = time.After(500 * time.Millisecond)
		case <-quiet:
			return
		}
	}
}
