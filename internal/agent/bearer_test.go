package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"

	"github.com/vbcherepanov/a2abridge/v4/internal/security"
)

const (
	outTok = "out-secret-91ab"
	inTok  = "in-secret-77cd"
)

// clearTokenEnv makes the token variables empty for one test.
func clearTokenEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"A2A_PEER_TOKENS", "A2A_PEER_TOKENS_FILE", "A2A_AUTH_TOKENS", "A2A_AUTH_TOKENS_FILE"} {
		t.Setenv(k, "")
	}
}

func mustPeerTokens(t *testing.T, value string) *PeerTokens {
	t.Helper()
	clearTokenEnv(t)
	t.Setenv("A2A_PEER_TOKENS", value)
	pt, err := LoadPeerTokens()
	if err != nil {
		t.Fatalf("LoadPeerTokens: %v", err)
	}
	return pt
}

func mustAuthTokens(t *testing.T, value string) *AuthTokens {
	t.Helper()
	clearTokenEnv(t)
	t.Setenv("A2A_AUTH_TOKENS", value)
	at, err := LoadAuthTokens()
	if err != nil {
		t.Fatalf("LoadAuthTokens: %v", err)
	}
	return at
}

// C-B6, C-B9, C-B12: configuration errors name the variable and the entry,
// never the token.
func TestTokenConfigErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "tokens")
	if err := os.WriteFile(file, []byte("a:"+inTok), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		env  map[string]string
		load func() error
		want string
	}{
		{"plain http off loopback", map[string]string{"A2A_PEER_TOKENS": "http://10.0.0.5:9000=" + outTok}, peerLoadErr, "plain HTTP"},
		{"bare outbound token", map[string]string{"A2A_PEER_TOKENS": "x=ok," + outTok}, peerLoadErr, "entry 2"},
		{"empty outbound token", map[string]string{"A2A_PEER_TOKENS": "hermes="}, peerLoadErr, "entry 1"},
		{"url with credentials", map[string]string{"A2A_PEER_TOKENS": "https://u:p@h.example=" + outTok}, peerLoadErr, "entry 1"},
		{"bare inbound token", map[string]string{"A2A_AUTH_TOKENS": inTok}, authLoadErr, "entry 1"},
		{"empty inbound name", map[string]string{"A2A_AUTH_TOKENS": ":" + inTok}, authLoadErr, "entry 1"},
		{"both set", map[string]string{"A2A_AUTH_TOKENS": "a:" + inTok, "A2A_AUTH_TOKENS_FILE": file}, authLoadErr, "A2A_AUTH_TOKENS_FILE"},
		{"unreadable file", map[string]string{"A2A_PEER_TOKENS_FILE": filepath.Join(dir, "missing")}, peerLoadErr, "A2A_PEER_TOKENS_FILE"},
		{"only separators", map[string]string{"A2A_AUTH_TOKENS": " , "}, authLoadErr, "no entries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearTokenEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			err := tc.load()
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), outTok) || strings.Contains(err.Error(), inTok) {
				t.Errorf("error leaks a token: %q", err)
			}
		})
	}
}

func peerLoadErr() error { _, err := LoadPeerTokens(); return err }
func authLoadErr() error { _, err := LoadAuthTokens(); return err }

// C-B9: unset means off; files take commas, newlines, comments and blank lines.
func TestTokenConfigSources(t *testing.T) {
	clearTokenEnv(t)
	if pt, err := LoadPeerTokens(); pt != nil || err != nil {
		t.Fatalf("unset A2A_PEER_TOKENS = %v, %v; want nil, nil", pt, err)
	}
	if at, err := LoadAuthTokens(); at != nil || err != nil {
		t.Fatalf("unset A2A_AUTH_TOKENS = %v, %v; want nil, nil", at, err)
	}

	file := filepath.Join(t.TempDir(), "tokens")
	content := "# peers\n\nalice:a1, bob:b1\n  # indented comment\ncarol:c:with:colons\n"
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2A_AUTH_TOKENS_FILE", file)
	at, err := LoadAuthTokens()
	if err != nil {
		t.Fatal(err)
	}
	for tok, want := range map[string]string{"a1": "alice", "b1": "bob", "c:with:colons": "carol"} {
		if name, ok := at.match(tok); !ok || name != want {
			t.Errorf("match(%q) = %q, %v; want %q", tok, name, ok, want)
		}
	}
	for _, bad := range []string{"", "a", "a1 ", "b1x", "alice"} {
		if _, ok := at.match(bad); ok {
			t.Errorf("match(%q) accepted", bad)
		}
	}
}

// C-B1, C-B2, C-B7: which request carries which token.
func TestPeerTokenMatching(t *testing.T) {
	pt := mustPeerTokens(t, strings.Join([]string{
		"https://hermes.example=root",
		"https://hermes.example/a2a=a2a",
		"http://127.0.0.1:9000=loop",
		"hermes=byname",
	}, ","))
	cases := []struct {
		url, want string
	}{
		{"https://hermes.example/", "root"},
		{"https://HERMES.example:443/x", "root"},
		{"https://hermes.example/a2a", "a2a"},
		{"https://hermes.example/a2a/.well-known/agent-card.json", "a2a"},
		{"https://hermes.example/a2ab", "root"},
		{"http://127.0.0.1:9000/", "loop"},
		{"http://127.0.0.1:9001/", ""},
		{"http://hermes.example/", ""},
		{"https://hermes.example:8443/", ""},
		{"https://evil.example/", ""},
	}
	for _, tc := range cases {
		u, _ := url.Parse(tc.url)
		got, _ := pt.forURL(u)
		if got != tc.want {
			t.Errorf("forURL(%s) = %q, want %q", tc.url, got, tc.want)
		}
	}
	if tok, ok := pt.forName("hermes"); !ok || tok != "byname" {
		t.Errorf("forName(hermes) = %q, %v", tok, ok)
	}
	if _, ok := pt.forName("Hermes"); ok {
		t.Error("name keys are case sensitive, like card names")
	}
}

// recordingPeer wraps a test agent's handler and records each request's
// Authorization header.
type recordingPeer struct {
	*testAgent
	mu    sync.Mutex
	auths map[string][]string // "METHOD path" -> headers
}

func newRecordingPeer(t *testing.T, name string, wrap func(http.Handler) http.Handler) *recordingPeer {
	t.Helper()
	rp := &recordingPeer{auths: map[string][]string{}}
	rp.testAgent = newTestAgentWithServer(t, name, func(s *http.Server) {
		next := s.Handler
		if wrap != nil {
			next = wrap(next)
		}
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rp.mu.Lock()
			rp.auths[r.Method+" "+r.URL.Path] = append(rp.auths[r.Method+" "+r.URL.Path], r.Header.Get("Authorization"))
			rp.mu.Unlock()
			next.ServeHTTP(w, r)
		})
	})
	return rp
}

func (rp *recordingPeer) postAuth() []string {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return append([]string(nil), rp.auths["POST /"]...)
}

func (rp *recordingPeer) anyAuth() bool {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	for _, hs := range rp.auths {
		for _, h := range hs {
			if h != "" {
				return true
			}
		}
	}
	return false
}

func sendVia(t *testing.T, peers *Peers, peerURL string) error {
	t.Helper()
	c, err := peers.Client(context.Background(), peerURL)
	if err != nil {
		return err
	}
	defer peers.Release(c)
	_, err = c.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: userMessage("hi"), Config: &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	return err
}

// C-B1, C-B2: URL keys and name keys reach the configured peer only.
func TestPeersSendBearer(t *testing.T) {
	hermes := newRecordingPeer(t, "hermes", nil)
	other := newRecordingPeer(t, "other", nil)
	peers := NewPeers(nil, mustPeerTokens(t, hermes.url+"="+outTok), discardLog())
	if err := sendVia(t, peers, hermes.url); err != nil {
		t.Fatal(err)
	}
	if err := sendVia(t, peers, other.url); err != nil {
		t.Fatal(err)
	}
	if got := hermes.postAuth(); len(got) == 0 || got[0] != "Bearer "+outTok {
		t.Errorf("configured peer got %q", got)
	}
	if other.anyAuth() {
		t.Error("peer without a token got an Authorization header")
	}

	byName := newRecordingPeer(t, "hermes", nil)
	peers = NewPeers(nil, mustPeerTokens(t, "hermes=n-"+outTok), discardLog())
	if err := sendVia(t, peers, byName.url); err != nil {
		t.Fatal(err)
	}
	if got := byName.postAuth(); len(got) == 0 || got[0] != "Bearer n-"+outTok {
		t.Errorf("name-keyed peer got %q", got)
	}

	both := newRecordingPeer(t, "hermes", nil)
	peers = NewPeers(nil, mustPeerTokens(t, "hermes=name,"+both.url+"=url"), discardLog())
	if err := sendVia(t, peers, both.url); err != nil {
		t.Fatal(err)
	}
	if got := both.postAuth(); len(got) == 0 || got[0] != "Bearer url" {
		t.Errorf("URL key should win over name key, got %q", got)
	}
}

// C-B7: a redirect to another origin carries no token.
func TestPeersNoTokenAcrossRedirect(t *testing.T) {
	target := newRecordingPeer(t, "target", nil)
	redirecting := newRecordingPeer(t, "hermes", func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				http.Redirect(w, r, target.url+"/", http.StatusTemporaryRedirect)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	peers := NewPeers(nil, mustPeerTokens(t, redirecting.url+"="+outTok+",hermes=n-"+outTok), discardLog())
	_ = sendVia(t, peers, redirecting.url)
	if got := redirecting.postAuth(); len(got) == 0 || got[0] != "Bearer "+outTok {
		t.Fatalf("redirecting peer got %q", got)
	}
	if len(target.postAuth()) == 0 {
		t.Fatal("redirect not followed")
	}
	if target.anyAuth() {
		t.Error("redirect target received the token")
	}
}

// C-B6: a token is refused over plain HTTP to a non-loopback host, before
// any byte leaves the bridge.
func TestBearerTransportRefusesPlainHTTP(t *testing.T) {
	called := false
	tr := &bearerTransport{
		next:       roundTripperFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("unreachable") }),
		nameOrigin: "http://hermes.lan:80", nameToken: outTok,
	}
	req, _ := http.NewRequest(http.MethodPost, "http://hermes.lan/", strings.NewReader("{}"))
	_, err := tr.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("err = %v, want a plain HTTP refusal", err)
	}
	if strings.Contains(err.Error(), outTok) {
		t.Errorf("refusal leaks the token: %v", err)
	}
	if called {
		t.Error("the request reached the network")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("RoundTrip modified the caller's request")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// syncLog is a slog handler target safe for concurrent writes.
type syncLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func doReq(t *testing.T, method, url, token string) *http.Response {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`)
	} else {
		body = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp
}

// C-B3, C-B4, C-B8, C-B11: what needs a token, what stays open, and that the
// token never reaches the log.
func TestInboundBearerGuard(t *testing.T) {
	logs := &syncLog{}
	a := newAuthTestAgent(t, "self", mustAuthTokens(t, "hermes:"+inTok), slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), nil)

	for name, h := range map[string]string{"none": "", "wrong": "Bearer nope", "basic": "Basic " + inTok, "empty": "Bearer "} {
		resp := doReq(t, http.MethodPost, a.url+"/", h)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("POST / with %s: %d %q, want 401 with WWW-Authenticate: Bearer", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	for _, path := range []string{"/tasks", "/tasks/x", "/metrics"} {
		if resp := doReq(t, http.MethodGet, a.url+path, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without token: %d, want 401", path, resp.StatusCode)
		}
	}
	if resp := doReq(t, http.MethodGet, a.url+"/metrics", "bearer "+inTok); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics with token (lower-case scheme): %d, want 200", resp.StatusCode)
	}
	for _, path := range []string{"/healthz", a2asrv.WellKnownAgentCardPath} {
		if resp := doReq(t, http.MethodGet, a.url+path, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s without token: %d, want 200", path, resp.StatusCode)
		}
	}

	card, err := NewPeers(nil, nil, discardLog()).Resolve(context.Background(), a.url)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := card.SecuritySchemes[bearerSchemeName].(a2a.HTTPAuthSecurityScheme); !ok || len(card.SecurityRequirements) != 1 {
		t.Errorf("card security = %v / %v", card.SecuritySchemes, card.SecurityRequirements)
	}

	// C-B5: the token's name reaches the inbox next to the claimed from.
	peers := NewPeers(nil, mustPeerTokens(t, a.url+"="+inTok), discardLog())
	if err := sendVia(t, peers, a.url); err != nil {
		t.Fatalf("authenticated send: %v", err)
	}
	e := a.firstInboxEntry(t)
	if e.From != "peer-a" || e.AuthName != "hermes" {
		t.Errorf("inbox entry from=%q authName=%q, want peer-a / hermes", e.From, e.AuthName)
	}
	if strings.Contains(logs.String(), inTok) {
		t.Error("the log contains the inbound token")
	}
}

// C-B5: an unauthenticated sender has no authName, and the name survives a
// restart through the inbox snapshot.
func TestAuthNameInboxRoundTrip(t *testing.T) {
	a := newTestAgent(t, "self")
	if err := sendVia(t, NewPeers(nil, nil, discardLog()), a.url); err != nil {
		t.Fatal(err)
	}
	if e := a.firstInboxEntry(t); e.AuthName != "" {
		t.Errorf("unauthenticated entry has authName %q", e.AuthName)
	}

	path := filepath.Join(t.TempDir(), "inbox.json")
	s := NewStore()
	t.Cleanup(s.Close)
	s.InboxPath = path
	s.deliverIncoming(&InboxEntry{MessageID: "m1", TaskID: "t1", From: "alice", AuthName: "hermes", Text: "x"})
	restored := NewStore()
	t.Cleanup(restored.Close)
	restored.InboxPath = path
	restored.LoadInbox()
	if got := restored.PeekInbox(); len(got) != 1 || got[0].AuthName != "hermes" {
		t.Errorf("restored inbox = %+v, want authName hermes", got)
	}
}

// C-B10: with mTLS on, a request needs a client cert and a token.
func TestInboundBearerWithMTLS(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := security.GenerateEd25519Cert(dir, "peer")
	if err != nil {
		t.Fatal(err)
	}
	fed := security.FederationConfig{CertFile: certPath, KeyFile: keyPath, TrustRoots: []string{certPath}}
	srvTLS, err := fed.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	log := discardLog()
	store := NewStore()
	t.Cleanup(store.Close)
	pushConfigs := push.NewInMemoryStore()
	tasks := NewTTLStore(pushConfigs, log)
	t.Cleanup(tasks.Close)
	sender := NewPushSender(nil, true, log)
	t.Cleanup(func() { closePushSender(t, sender) })
	card := &a2a.AgentCard{Name: "self", Capabilities: a2a.AgentCapabilities{Streaming: true}}
	handler, err := NewA2AHTTPHandler(card, NewExecutor(store, log), tasks, pushConfigs, sender, mustAuthTokens(t, "hermes:"+inTok), log)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, TLSConfig: srvTLS, ReadHeaderTimeout: waitTimeout}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	url := "https://" + ln.Addr().String() + "/"

	pem, _ := os.ReadFile(certPath)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	client := func(withCert bool) *http.Client {
		cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
		if withCert {
			cfg.Certificates = []tls.Certificate{pair}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	}
	post := func(c *http.Client, token string) (int, error) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := post(client(true), ""); err != nil || code != http.StatusUnauthorized {
		t.Errorf("cert without token: %d, %v; want 401", code, err)
	}
	if code, err := post(client(true), inTok); err != nil || code == http.StatusUnauthorized {
		t.Errorf("cert with token: %d, %v; want it served", code, err)
	}
	if _, err := post(client(false), inTok); err == nil {
		t.Error("token without cert was served; want a TLS handshake failure")
	}
}
