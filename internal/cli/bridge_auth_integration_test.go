//go:build integration

// Acceptance tests for bearer-token auth (#47). They run the real bridge
// binary with A2A_PEER_TOKENS / A2A_AUTH_TOKENS set and drive it the way an
// IDE (MCP over stdio) and a remote peer (A2A over HTTP) do:
//
//	go test -tags=integration ./internal/cli/ -run TestAuth -v
package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/vbcherepanov/a2abridge/v4/internal/security"
)

const (
	peerToken    = "peer-secret-7f3a9c"
	inboundToken = "inbound-secret-c41d2e"
)

// syncBuffer is a bytes.Buffer safe to write from exec's copier goroutine.
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

// bridgeProc is a running bridge with its MCP stdio pipes.
type bridgeProc struct {
	t        *testing.T
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	lines    chan string
	stderr   *syncBuffer
	stateDir string
	port     int
	nextID   int
}

// bridgeEnv returns the process environment plus extra, with every token
// variable cleared first so the developer's own settings never leak in.
func bridgeEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "A2A_PEER_TOKENS", "A2A_PEER_TOKENS_FILE", "A2A_AUTH_TOKENS", "A2A_AUTH_TOKENS_FILE",
			"A2A_TLS_CERT", "A2A_TLS_KEY", "A2A_TRUST_ROOTS", "A2A_PEER_ALLOW", "A2A_NUDGE", "A2A_RESPONDER":
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func startAuthBridge(t *testing.T, stateDir string, env ...string) *bridgeProc {
	t.Helper()
	cmd := exec.Command(bridgeBin, bridgeArgs("127.0.0.1:0", stateDir, "auth-itest")...)
	cmd.Env = bridgeEnv(env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	b := &bridgeProc{t: t, cmd: cmd, stdin: stdin, lines: make(chan string, 64), stderr: stderr, stateDir: stateDir}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			b.lines <- sc.Text()
		}
		close(b.lines)
	}()
	b.port = waitForPort(t, stateDir)
	b.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "auth-itest", "version": "0"},
	})
	b.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return b
}

func (b *bridgeProc) url() string { return fmt.Sprintf("http://127.0.0.1:%d", b.port) }

func (b *bridgeProc) send(msg map[string]any) {
	b.t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := b.stdin.Write(append(raw, '\n')); err != nil {
		b.t.Fatalf("write mcp stdin: %v", err)
	}
}

// rpc sends one MCP request and returns its result, skipping notifications.
func (b *bridgeProc) rpc(method string, params any) json.RawMessage {
	b.t.Helper()
	b.nextID++
	id := b.nextID
	b.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.After(20 * time.Second)
	for {
		select {
		case line, ok := <-b.lines:
			if !ok {
				b.t.Fatalf("bridge closed stdout before answering %s; stderr: %s", method, b.stderr.String())
			}
			var resp struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal([]byte(line), &resp) != nil || resp.ID == nil || *resp.ID != id {
				continue
			}
			if resp.Error != nil {
				b.t.Fatalf("%s: mcp error %s", method, resp.Error)
			}
			return resp.Result
		case <-deadline:
			b.t.Fatalf("no answer to %s within 20s", method)
		}
	}
}

// tool calls an MCP tool and returns its text and whether it is an error.
func (b *bridgeProc) tool(name string, args map[string]any) (string, bool) {
	b.t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(b.rpc("tools/call", map[string]any{"name": name, "arguments": args}), &res); err != nil {
		b.t.Fatal(err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		text.WriteString(c.Text)
	}
	return text.String(), res.IsError
}

// logText returns the bridge log plus stderr, for leak and message checks.
func (b *bridgeProc) logText() string {
	raw, _ := os.ReadFile(filepath.Join(b.stateDir, "bridge.log"))
	return string(raw) + b.stderr.String()
}

// runBridgeExpectRefusal starts a bridge that must refuse to start. It returns
// the bridge's log and stderr. A bridge still running after 10s fails the test.
func runBridgeExpectRefusal(t *testing.T, env ...string) string {
	t.Helper()
	stateDir := t.TempDir()
	cmd := exec.Command(bridgeBin, bridgeArgs("127.0.0.1:0", stateDir, "refuse-itest")...)
	cmd.Env = bridgeEnv(env...)
	stdin, err := cmd.StdinPipe() // held open: only a refusal makes it exit
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("bridge exited 0, want a non-zero refusal; stderr: %s", stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("bridge kept running; want it to refuse to start")
	}
	raw, _ := os.ReadFile(filepath.Join(stateDir, "bridge.log"))
	return string(raw) + stderr.String()
}

// seenRequest is one HTTP request a fake peer received.
type seenRequest struct {
	Method, Host, Path, Auth string
}

// fakePeer is a minimal A2A agent that records every request's
// Authorization header.
type fakePeer struct {
	url  string
	mu   sync.Mutex
	seen []seenRequest
}

type fakePeerOpts struct {
	name       string
	ifaceURL   string // interface URL in the card; default: the peer itself
	redirectTo string // when set, protocol POSTs get a 307 to this URL
}

type replyExecutor struct{}

func (replyExecutor) Execute(_ context.Context, _ *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("ok")), nil)
	}
}

func (replyExecutor) Cancel(_ context.Context, _ *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

func newFakePeer(t *testing.T, o fakePeerOpts) *fakePeer {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	fp := &fakePeer{url: "http://" + srv.Listener.Addr().String()}
	iface := o.ifaceURL
	if iface == "" {
		iface = fp.url
	}
	card := &a2a.AgentCard{
		Name:                o.name,
		Description:         "fake peer",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(iface, a2a.TransportProtocolJSONRPC)},
		Version:             "test",
		Capabilities:        a2a.AgentCapabilities{},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2a.AgentSkill{},
	}
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("POST /{$}", a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(replyExecutor{})))
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.mu.Lock()
		fp.seen = append(fp.seen, seenRequest{Method: r.Method, Host: r.Host, Path: r.URL.Path, Auth: r.Header.Get("Authorization")})
		fp.mu.Unlock()
		if o.redirectTo != "" && r.Method == http.MethodPost {
			http.Redirect(w, r, o.redirectTo, http.StatusTemporaryRedirect)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv.Start()
	t.Cleanup(srv.Close)
	return fp
}

func (fp *fakePeer) requests() []seenRequest {
	fp.mu.Lock()
	defer fp.mu.Unlock()
	return append([]seenRequest(nil), fp.seen...)
}

// protocolAuth returns the Authorization headers of the protocol POSTs.
func (fp *fakePeer) protocolAuth() []string {
	var out []string
	for _, r := range fp.requests() {
		if r.Method == http.MethodPost {
			out = append(out, r.Auth)
		}
	}
	return out
}

func sendTo(t *testing.T, b *bridgeProc, peerURL string) (string, bool) {
	t.Helper()
	return b.tool("a2a_send_message", map[string]any{"peer_url": peerURL, "text": "hello", "blocking": true})
}

func assertNoTokenIn(t *testing.T, what, text string, tokens ...string) {
	t.Helper()
	for _, tok := range tokens {
		if strings.Contains(text, tok) {
			t.Errorf("%s contains a token: %q", what, text)
		}
	}
}

// C-B1: a URL key sends Authorization: Bearer to that peer only.
func TestAuthOutboundURLKey(t *testing.T) {
	peer := newFakePeer(t, fakePeerOpts{name: "hermes"})
	other := newFakePeer(t, fakePeerOpts{name: "other"})
	b := startAuthBridge(t, t.TempDir(), "A2A_PEER_TOKENS="+peer.url+"="+peerToken)

	if text, isErr := sendTo(t, b, peer.url); isErr {
		t.Fatalf("send to configured peer failed: %s", text)
	}
	auths := peer.protocolAuth()
	if len(auths) == 0 || auths[0] != "Bearer "+peerToken {
		t.Errorf("configured peer got Authorization %q, want %q", auths, "Bearer "+peerToken)
	}

	if text, isErr := sendTo(t, b, other.url); isErr {
		t.Fatalf("send to other peer failed: %s", text)
	}
	for _, r := range other.requests() {
		if r.Auth != "" {
			t.Errorf("peer with no token got Authorization %q on %s %s", r.Auth, r.Method, r.Path)
		}
	}
	assertNoTokenIn(t, "bridge log", b.logText(), peerToken)
}

// C-B2: a name key matches the peer's card name, pinned to the peer_url origin;
// a URL key wins over a name key.
func TestAuthOutboundNameKey(t *testing.T) {
	hermes := newFakePeer(t, fakePeerOpts{name: "hermes"})
	b := startAuthBridge(t, t.TempDir(), "A2A_PEER_TOKENS=hermes="+peerToken)
	if text, isErr := sendTo(t, b, hermes.url); isErr {
		t.Fatalf("send failed: %s", text)
	}
	if auths := hermes.protocolAuth(); len(auths) == 0 || auths[0] != "Bearer "+peerToken {
		t.Errorf("name-matched peer got Authorization %q, want the name key's token", auths)
	}

	both := newFakePeer(t, fakePeerOpts{name: "hermes"})
	b2 := startAuthBridge(t, t.TempDir(), "A2A_PEER_TOKENS=hermes=name-token,"+both.url+"="+peerToken)
	if text, isErr := sendTo(t, b2, both.url); isErr {
		t.Fatalf("send failed: %s", text)
	}
	if auths := both.protocolAuth(); len(auths) == 0 || auths[0] != "Bearer "+peerToken {
		t.Errorf("peer matching both keys got %q, want the URL key's token", auths)
	}
}

// C-B6: a name-keyed token is refused over plain HTTP to a non-loopback host,
// and the tool error says why. The peer is reached through an HTTP proxy so
// the test needs no real remote host.
func TestAuthOutboundRefusesPlainHTTPOffLoopback(t *testing.T) {
	proxy := newFakePeer(t, fakePeerOpts{name: "hermes", ifaceURL: "http://hermes.test/"})
	b := startAuthBridge(t, t.TempDir(),
		"A2A_PEER_TOKENS=hermes="+peerToken,
		"HTTP_PROXY="+proxy.url, "http_proxy="+proxy.url, "NO_PROXY=", "no_proxy=")

	text, isErr := sendTo(t, b, "http://hermes.test/")
	if !isErr {
		t.Fatalf("send over plain HTTP off loopback succeeded, want a refusal; result: %s", text)
	}
	if !strings.Contains(strings.ToLower(text), "plain http") {
		t.Errorf("refusal %q does not say it was refused for plain HTTP", text)
	}
	for _, r := range proxy.requests() {
		if r.Auth != "" {
			t.Errorf("token left the bridge over plain HTTP: %s %s %s", r.Method, r.Host, r.Path)
		}
	}
	assertNoTokenIn(t, "tool error", text, peerToken)
}

// C-B7: no token across a redirect to another origin, and none to a card
// interface URL on another origin.
func TestAuthOutboundNoTokenAcrossOrigins(t *testing.T) {
	target := newFakePeer(t, fakePeerOpts{name: "target"})

	redirecting := newFakePeer(t, fakePeerOpts{name: "hermes", redirectTo: target.url + "/"})
	b := startAuthBridge(t, t.TempDir(), "A2A_PEER_TOKENS="+redirecting.url+"="+peerToken)
	_, _ = sendTo(t, b, redirecting.url)
	if auths := redirecting.protocolAuth(); len(auths) == 0 || auths[0] != "Bearer "+peerToken {
		t.Fatalf("configured peer got %q before redirecting, want the token", auths)
	}
	posts := 0
	for _, r := range target.requests() {
		if r.Method == http.MethodPost {
			posts++
		}
		if r.Auth != "" {
			t.Errorf("redirect target got Authorization on %s %s", r.Method, r.Path)
		}
	}
	if posts == 0 {
		t.Fatalf("redirect was not followed; the test proves nothing")
	}

	elsewhere := newFakePeer(t, fakePeerOpts{name: "elsewhere"})
	pointing := newFakePeer(t, fakePeerOpts{name: "hermes", ifaceURL: elsewhere.url})
	b2 := startAuthBridge(t, t.TempDir(), "A2A_PEER_TOKENS="+pointing.url+"="+peerToken)
	_, _ = sendTo(t, b2, pointing.url)
	if len(elsewhere.protocolAuth()) == 0 {
		t.Fatalf("card interface URL was not used; the test proves nothing")
	}
	for _, r := range elsewhere.requests() {
		if r.Auth != "" {
			t.Errorf("interface on another origin got Authorization on %s %s", r.Method, r.Path)
		}
	}
}

func bearerClient(token string, tlsCfg *tls.Config) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = tlsCfg
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if token != "" {
			r = r.Clone(r.Context())
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return base.RoundTrip(r)
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// httpReply is what a test reads from a response whose body it closed.
type httpReply struct {
	StatusCode int
	Header     http.Header
}

func jsonRPCGetTask(t *testing.T, c *http.Client, url, token string) httpReply {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url+"/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"nope"}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("A2A-Version", "1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return httpReply{StatusCode: resp.StatusCode, Header: resp.Header}
}

func get(t *testing.T, c *http.Client, url string) httpReply {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return httpReply{StatusCode: resp.StatusCode, Header: resp.Header}
}

// C-B3, C-B4, C-B11: inbound tokens guard the protocol and /metrics; the card
// and /healthz stay open; the card advertises the bearer scheme.
func TestAuthInboundGuardsEndpoints(t *testing.T) {
	b := startAuthBridge(t, t.TempDir(), "A2A_AUTH_TOKENS=hermes:"+inboundToken)
	c := http.DefaultClient

	for name, tok := range map[string]string{"no token": "", "wrong token": "wrong-token-9b2f"} {
		resp := jsonRPCGetTask(t, c, b.url(), tok)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("JSON-RPC with %s: status %d, want 401", name, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
			t.Errorf("JSON-RPC with %s: WWW-Authenticate %q, want Bearer", name, got)
		}
	}
	if resp := jsonRPCGetTask(t, c, b.url(), inboundToken); resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("JSON-RPC with the right token: 401")
	}
	if resp := get(t, c, b.url()+"/tasks"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("REST /tasks without token: status %d, want 401", resp.StatusCode)
	}
	if resp := get(t, c, b.url()+"/metrics"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/metrics without token: status %d, want 401", resp.StatusCode)
	}
	if resp := get(t, bearerClient(inboundToken, nil), b.url()+"/metrics"); resp.StatusCode != http.StatusOK {
		t.Errorf("/metrics with token: status %d, want 200", resp.StatusCode)
	}
	if resp := get(t, c, b.url()+"/healthz"); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz without token: status %d, want 200", resp.StatusCode)
	}

	resp, err := c.Get(b.url() + a2asrv.WellKnownAgentCardPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Agent Card without token: status %d, want 200", resp.StatusCode)
	}
	var card a2a.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatal(err)
	}
	var bearer bool
	for _, s := range card.SecuritySchemes {
		if h, ok := s.(a2a.HTTPAuthSecurityScheme); ok && strings.EqualFold(h.Scheme, "bearer") {
			bearer = true
		}
	}
	if !bearer || len(card.SecurityRequirements) == 0 {
		t.Errorf("card securitySchemes %v / requirements %v, want a bearer httpAuth scheme and a requirement", card.SecuritySchemes, card.SecurityRequirements)
	}
	assertNoTokenIn(t, "bridge log", b.logText(), inboundToken, "wrong-token-9b2f")
}

// C-B13: with no token variables set the card carries no security scheme and
// the protocol stays open, as before.
func TestAuthOffByDefault(t *testing.T) {
	b := startAuthBridge(t, t.TempDir())
	if resp := jsonRPCGetTask(t, http.DefaultClient, b.url(), ""); resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("JSON-RPC without tokens configured: 401")
	}
	card, err := agentcard.NewResolver(http.DefaultClient).Resolve(context.Background(), b.url())
	if err != nil {
		t.Fatal(err)
	}
	if len(card.SecuritySchemes) != 0 || len(card.SecurityRequirements) != 0 {
		t.Errorf("card has security %v / %v with no tokens set", card.SecuritySchemes, card.SecurityRequirements)
	}
}

// C-B5: the token's name reaches the agent next to the self-reported from, in
// a2a_inbox and in the prompt hook.
func TestAuthInboundSenderIdentity(t *testing.T) {
	project := t.TempDir()
	b := startAuthBridge(t, filepath.Join(project, ".a2a"), "A2A_AUTH_TOKENS=hermes:"+inboundToken)

	hc := bearerClient(inboundToken, nil)
	card, err := agentcard.NewResolver(hc).Resolve(context.Background(), b.url())
	if err != nil {
		t.Fatal(err)
	}
	client, err := a2aclient.NewFromCard(context.Background(), card, a2aclient.WithJSONRPCTransport(hc))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Destroy() }()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("deploy please"))
	msg.Metadata = map[string]any{"from": "alice"}
	if _, err := client.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true},
	}); err != nil {
		t.Fatalf("authenticated send: %v", err)
	}

	var entries []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		text, _ := b.tool("a2a_inbox", map[string]any{"peek": true})
		entries = nil
		_ = json.Unmarshal([]byte(text), &entries)
		if len(entries) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(entries) == 0 {
		t.Fatal("message never reached the inbox")
	}
	if entries[0]["from"] != "alice" || entries[0]["authName"] != "hermes" {
		t.Errorf("a2a_inbox entry from=%v authName=%v, want from=alice authName=hermes", entries[0]["from"], entries[0]["authName"])
	}

	hook := exec.Command("bash", "../assets/hook/a2a-inbox-hook.sh")
	hook.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+project, "HOME="+t.TempDir())
	out, err := hook.Output()
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if !strings.Contains(string(out), "`alice` (claimed)") || !strings.Contains(string(out), "token `hermes`") {
		t.Errorf("hook output does not show the claimed sender and the token name: %s", out)
	}
	assertNoTokenIn(t, "hook output", string(out), inboundToken)
	assertNoTokenIn(t, "bridge log", b.logText(), inboundToken)
}

// C-B10: with mTLS on, a request needs a valid client cert AND a valid token.
func TestAuthInboundWithMTLSNeedsBoth(t *testing.T) {
	certDir := t.TempDir()
	certPath, keyPath, err := security.GenerateEd25519Cert(certDir, "itest")
	if err != nil {
		t.Fatal(err)
	}
	b := startAuthBridge(t, t.TempDir(),
		"A2A_AUTH_TOKENS=hermes:"+inboundToken,
		"A2A_TLS_CERT="+certPath, "A2A_TLS_KEY="+keyPath, "A2A_TRUST_ROOTS="+certPath)
	url := fmt.Sprintf("https://127.0.0.1:%d", b.port)

	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	withCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}}}
	noCert := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}}

	if resp := jsonRPCGetTask(t, withCert, url, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("client cert without token: status %d, want 401", resp.StatusCode)
	}
	if resp := jsonRPCGetTask(t, withCert, url, inboundToken); resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("client cert with token: 401")
	}
	req, _ := http.NewRequest(http.MethodPost, url+"/", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+inboundToken)
	if resp, err := noCert.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Errorf("token without client cert got status %d, want a TLS handshake failure", resp.StatusCode)
	}
}

// C-B6, C-B9, C-B12: bad configuration refuses to start, says why, and never
// prints the token.
func TestAuthRefusesBadConfig(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenFile, []byte("hermes:"+inboundToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"plain http off loopback", []string{"A2A_PEER_TOKENS=http://10.1.2.3:9000=" + peerToken}, "plain http"},
		{"bare outbound token", []string{"A2A_PEER_TOKENS=" + peerToken}, "entry 1"},
		{"bare inbound token", []string{"A2A_AUTH_TOKENS=hermes:ok," + inboundToken}, "entry 2"},
		{"empty inbound name", []string{"A2A_AUTH_TOKENS=:" + inboundToken}, "entry 1"},
		{"empty outbound token", []string{"A2A_PEER_TOKENS=hermes="}, "entry 1"},
		{"variable and file both set", []string{"A2A_AUTH_TOKENS=hermes:" + inboundToken, "A2A_AUTH_TOKENS_FILE=" + tokenFile}, "A2A_AUTH_TOKENS_FILE"},
		{"unreadable file", []string{"A2A_PEER_TOKENS_FILE=" + filepath.Join(t.TempDir(), "missing")}, "A2A_PEER_TOKENS_FILE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runBridgeExpectRefusal(t, tc.env...)
			if !strings.Contains(strings.ToLower(out), strings.ToLower(tc.want)) {
				t.Errorf("refusal does not mention %q: %s", tc.want, out)
			}
			assertNoTokenIn(t, "refusal output", out, peerToken, inboundToken)
		})
	}
}

// C-B9: tokens load from a file with comments, blank lines and newlines.
func TestAuthTokensFromFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenFile, []byte("# inbound peers\n\nother:x1,ignored:x2\nhermes:"+inboundToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := startAuthBridge(t, t.TempDir(), "A2A_AUTH_TOKENS_FILE="+tokenFile)
	if resp := jsonRPCGetTask(t, http.DefaultClient, b.url(), ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token with a token file: status %d, want 401", resp.StatusCode)
	}
	if resp := jsonRPCGetTask(t, http.DefaultClient, b.url(), inboundToken); resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("token from the file's last line rejected")
	}
}
