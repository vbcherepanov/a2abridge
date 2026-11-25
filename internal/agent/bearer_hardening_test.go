package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Fix 1: a path-scoped URL key never follows dot segments or backslashes out
// of its path.
func TestURLKeyWithholdsUncleanPaths(t *testing.T) {
	pt := mustPeerTokens(t, "https://gw.example/a2a=T,https://root.example=R")
	cases := []struct {
		raw, want string
	}{
		{"https://gw.example/a2a/x", "T"},
		{"https://gw.example/a2a/", "T"},
		{"https://gw.example/a2a/../evil", ""},
		{"https://gw.example/a2a/%2e%2e/evil", ""},
		{"https://gw.example/a2a/%2E%2E/evil", ""},
		{"https://gw.example/a2a/./x", ""},
		{"https://gw.example/a2a//x", ""},
		{"https://gw.example/a2a/..%5cevil", ""},
		{"https://gw.example/a2a/x%5c..%5cevil", ""},
		// A root key covers the whole origin; dot segments cannot leave it.
		{"https://root.example/a/../b", "R"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := pt.forURL(u); got != tc.want {
			t.Errorf("forURL(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// Fix 4, 9, 10: keys and tokens that look like mistakes refuse to load.
func TestTokenConfigRefusesMistakes(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		load func() error
		want string
	}{
		{"name key with colon", map[string]string{"A2A_PEER_TOKENS": "hermes:9000=" + outTok}, peerLoadErr, "entry 1"},
		{"name key with slash", map[string]string{"A2A_PEER_TOKENS": "x=a,hermes/a2a=" + outTok}, peerLoadErr, "entry 2"},
		{"duplicate name key", map[string]string{"A2A_PEER_TOKENS": "hermes=a,x=b,hermes=c"}, peerLoadErr, "entries 1 and 3"},
		{"duplicate URL key", map[string]string{"A2A_PEER_TOKENS": "https://h.example/a2a=a,https://H.example:443/a2a/=b"}, peerLoadErr, "entries 1 and 2"},
		{"same inbound token twice", map[string]string{"A2A_AUTH_TOKENS": "alice:" + inTok + ",bob:x,carol:" + inTok}, authLoadErr, "entries 1 and 3"},
		{"inline comment", map[string]string{"A2A_AUTH_TOKENS": "alice:" + inTok + " # laptop"}, authLoadErr, "entry 1"},
		{"space in outbound token", map[string]string{"A2A_PEER_TOKENS": "hermes=ab cd"}, peerLoadErr, "entry 1"},
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
			for _, secret := range []string{outTok, inTok, "ab cd"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks a token: %q", err)
				}
			}
		})
	}
}

// Fix 10, 12: what must keep loading.
func TestTokenConfigAccepts(t *testing.T) {
	pt := mustPeerTokens(t, "\ufeffhttp://localhost:9000=L,https://h.example/=S,hermes=abc==")
	for raw, want := range map[string]string{
		"http://localhost:9000/x":         "L",
		"https://h.example":               "S",
		"https://h.example/message:send":  "S",
		"https://h.example/tasks/abc/get": "S",
	} {
		u, _ := url.Parse(raw)
		if got, _ := pt.forURL(u); got != want {
			t.Errorf("forURL(%s) = %q, want %q", raw, got, want)
		}
	}
	if tok, ok := pt.forName("hermes"); !ok || tok != "abc==" {
		t.Errorf("forName(hermes) = %q, %v; want the padded token", tok, ok)
	}

	file := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(file, []byte("\ufeffalice:a1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	clearTokenEnv(t)
	t.Setenv("A2A_AUTH_TOKENS_FILE", file)
	at, err := LoadAuthTokens()
	if err != nil {
		t.Fatalf("file with a BOM: %v", err)
	}
	if name, ok := at.match("a1"); !ok || name != "alice" {
		t.Errorf("BOM file: match(a1) = %q, %v", name, ok)
	}

	// The _FILE variant names itself when it holds nothing.
	if err := os.WriteFile(file, []byte("# nothing yet\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthTokens(); err == nil || !strings.Contains(err.Error(), "A2A_AUTH_TOKENS_FILE holds no entries") {
		t.Errorf("empty file error = %v, want it to name A2A_AUTH_TOKENS_FILE", err)
	}
}

// Fix 5: a startup summary lists keys, never tokens.
func TestPeerTokensDescribe(t *testing.T) {
	pt := mustPeerTokens(t, "https://h.example/a2a="+outTok+",hermes.example.com=n-"+outTok)
	urls, names := pt.Describe()
	if len(urls) != 1 || urls[0] != "https://h.example:443/a2a" {
		t.Errorf("urls = %v", urls)
	}
	if len(names) != 1 || names[0] != "hermes.example.com" {
		t.Errorf("names = %v", names)
	}
	if strings.Contains(strings.Join(append(urls, names...), " "), outTok) {
		t.Error("summary leaks a token")
	}
	if n := mustAuthTokens(t, "a:x,b:y").Count(); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

// Fix 6: a card that points at another origin does not get the token, and
// the call fails saying so.
func TestPeersRefuseCardOnAnotherOrigin(t *testing.T) {
	for _, keyKind := range []string{"url", "name"} {
		t.Run(keyKind, func(t *testing.T) {
			elsewhere := newRecordingPeer(t, "elsewhere", nil)
			// The card handler caches the card it was built with, so the
			// pointing card is served by a wrapper.
			var pointing *recordingPeer
			pointing = newRecordingPeer(t, "hermes", func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != a2asrv.WellKnownAgentCardPath {
						next.ServeHTTP(w, r)
						return
					}
					c := *pointing.card
					c.SupportedInterfaces = []*a2a.AgentInterface{a2a.NewAgentInterface(elsewhere.url, a2a.TransportProtocolJSONRPC)}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(&c)
				})
			})
			key := pointing.url + "=" + outTok
			if keyKind == "name" {
				key = "hermes=" + outTok
			}
			peers := NewPeers(nil, mustPeerTokens(t, key), discardLog())
			err := sendVia(t, peers, pointing.url)
			if err == nil || !strings.Contains(err.Error(), "another origin") {
				t.Fatalf("err = %v, want a refusal naming another origin", err)
			}
			if strings.Contains(err.Error(), outTok) {
				t.Errorf("error leaks the token: %v", err)
			}
			if elsewhere.anyAuth() {
				t.Error("the other origin received the token")
			}
		})
	}
}

// Fix 7: an inbound 401 leaves one throttled warning, without the token.
func TestInboundRejectionIsLogged(t *testing.T) {
	logs := &syncLog{}
	a := newAuthTestAgent(t, "self", mustAuthTokens(t, "hermes:"+inTok), slog.New(slog.NewTextHandler(logs, nil)), nil)
	doReq(t, http.MethodPost, a.url+"/", "Bearer wrong-"+inTok)
	doReq(t, http.MethodPost, a.url+"/", "")
	doReq(t, http.MethodGet, a.url+"/metrics", "Basic x")
	out := logs.String()
	if n := strings.Count(out, "bearer auth rejected"); n != 1 {
		t.Fatalf("%d rejection warnings, want exactly 1 (throttled): %s", n, out)
	}
	for _, want := range []string{"level=WARN", "path=/", "reason=\"unknown token\"", "remote=127.0.0.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, inTok) {
		t.Error("the warning leaks the token")
	}
}

// Fix 8: the on-inbound user hook gets authName in its payload and env.
func TestInboundHookCarriesAuthName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook script")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	hooks := filepath.Join(home, ".a2abridge", "hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(home, "env.txt")
	script := "#!/bin/sh\nenv > " + dump + ".tmp\ncat >> " + dump + ".tmp\nmv " + dump + ".tmp " + dump + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "on-inbound.sh"), []byte(script), 0o700); err != nil { //nolint:gosec // G306: test hook must be executable
		t.Fatal(err)
	}
	s := NewStore()
	t.Cleanup(s.Close)
	s.deliverIncoming(&InboxEntry{MessageID: "m1", TaskID: "t1", From: "alice", AuthName: "hermes", Text: "x"})
	var got string
	waitFor(t, "the on-inbound hook", func() bool {
		b, err := os.ReadFile(dump)
		got = string(b)
		return err == nil
	})
	if !strings.Contains(got, "A2A_EVENT_AUTH_NAME=hermes") {
		t.Errorf("hook env lacks A2A_EVENT_AUTH_NAME=hermes")
	}
	if !strings.Contains(got, `"authName":"hermes"`) {
		t.Errorf("hook payload lacks authName")
	}
}

// Fix 8: the responder prompt tells a verified name from a claimed one.
func TestResponderPromptLabelsSender(t *testing.T) {
	r := &Responder{Card: &a2a.AgentCard{Name: "self"}}
	p := r.buildPrompt(&InboxEntry{From: "alice", AuthName: "hermes"}, "q")
	if !strings.Contains(p, `"alice"`) || !strings.Contains(p, `"hermes"`) || !strings.Contains(p, "не проверено") || !strings.Contains(p, "проверен токеном") {
		t.Errorf("prompt does not label claimed and verified sender:\n%s", p)
	}
	if p := r.buildPrompt(&InboxEntry{From: "alice"}, "q"); strings.Contains(p, "токеном") {
		t.Errorf("prompt mentions a token for an unauthenticated sender:\n%s", p)
	}
}

// Fix 11: a subscription refused for missing credentials is a warning.
func TestSubscribeUnauthenticatedWarns(t *testing.T) {
	a := newAuthTestAgent(t, "self", mustAuthTokens(t, "hermes:"+inTok), discardLog(), nil)
	logs := &syncLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	s := NewStore()
	t.Cleanup(s.Close)
	subscribeOutgoingReply(context.Background(), NewPeers(nil, nil, discardLog()), a.url, "t1", s, log)
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("no warning for an unauthenticated subscription: %q", logs.String())
	}
}

// Codex 1: a peer cannot redirect our request to another configured peer
// and have it carry that peer's token (confused deputy).
func TestPeersRefuseRedirectIntoConfiguredPeer(t *testing.T) {
	// A carries no token, its own URL-keyed token, or a name-keyed one (which
	// goes through the per-name client).
	for _, aKey := range []string{"none", "url", "name"} {
		b := newRecordingPeer(t, "b", nil)
		a := newRecordingPeer(t, "a", func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					http.Redirect(w, r, b.url+"/", http.StatusTemporaryRedirect)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		keys := b.url + "=b-" + outTok
		switch aKey {
		case "url":
			keys += "," + a.url + "=a-" + outTok
		case "name":
			keys += ",a=a-" + outTok
		}
		err := sendVia(t, NewPeers(nil, mustPeerTokens(t, keys), discardLog()), a.url)
		if err == nil || !strings.Contains(err.Error(), "refused to follow a redirect") {
			t.Errorf("a key %s: err = %v, want a refused redirect", aKey, err)
		} else if strings.Contains(err.Error(), outTok) {
			t.Errorf("a key %s: error leaks a token", aKey)
		}
		if b.anyAuth() {
			t.Errorf("a key %s: redirect target received its token", aKey)
		}
	}
}

// Codex 2: a token variable that is set but empty refuses startup instead
// of turning auth off; unset stays off.
func TestTokenVariablesSetButEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		load        func() error
	}{
		{"A2A_AUTH_TOKENS", "", authLoadErr},
		{"A2A_AUTH_TOKENS", "  ", authLoadErr},
		{"A2A_PEER_TOKENS", "", peerLoadErr},
		{"A2A_AUTH_TOKENS_FILE", "", authLoadErr},
		{"A2A_PEER_TOKENS_FILE", " ", peerLoadErr},
	} {
		clearTokenEnv(t)
		t.Setenv(tc.name, tc.value)
		err := tc.load()
		if err == nil || !strings.Contains(err.Error(), tc.name+" is set but empty") {
			t.Errorf("%s=%q: err = %v, want a set-but-empty refusal naming it", tc.name, tc.value, err)
		}
	}
	clearTokenEnv(t)
	if err := authLoadErr(); err != nil {
		t.Errorf("unset A2A_AUTH_TOKENS: %v, want off without error", err)
	}
}

// Codex 3: a path-scoped token is withheld from a path with encoded
// separators, which a proxy may route differently from the decoded path.
func TestURLKeyWithholdsEncodedSeparators(t *testing.T) {
	pt := mustPeerTokens(t, "https://gw.example/a/b=T,https://root.example=R")
	for raw, want := range map[string]string{
		"https://gw.example/a/b/c":         "T",
		"https://gw.example/a%2Fb":         "",
		"https://gw.example/a/b/c%2Fd":     "",
		"https://gw.example/a/b%2F..%2Fx":  "",
		"https://root.example/x%2Fy":       "R",
		"https://gw.example/a/b/%E2%9C%93": "T", // a plain escaped character keeps the default encoding
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := pt.forURL(u); got != want {
			t.Errorf("forURL(%s) = %q, want %q", raw, got, want)
		}
	}
}
