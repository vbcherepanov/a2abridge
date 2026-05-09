package directory

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRegistryHTTPRoundTrip exercises the four HTTP routes that bridges
// rely on: register, list, heartbeat, unregister. We use httptest so the
// suite remains hermetic (no port allocation, no goroutine cleanup).
func TestRegistryHTTPRoundTrip(t *testing.T) {
	reg := New(nopLogger())
	ts := httptest.NewServer(reg.Routes())
	defer ts.Close()

	post := func(path, body string) int {
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	listURLs := func() []string {
		resp, err := http.Get(ts.URL + "/agents")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out []Entry
		_ = json.NewDecoder(resp.Body).Decode(&out)
		urls := make([]string, 0, len(out))
		for _, e := range out {
			urls = append(urls, e.URL)
		}
		return urls
	}

	if got := post("/register", `{"url":"http://peer-a"}`); got != 204 {
		t.Errorf("register peer-a status = %d, want 204", got)
	}
	if got := post("/register", `{"url":"http://peer-b"}`); got != 204 {
		t.Errorf("register peer-b status = %d, want 204", got)
	}

	urls := listURLs()
	if len(urls) != 2 {
		t.Fatalf("got %d peers, want 2: %v", len(urls), urls)
	}

	if got := post("/heartbeat", `{"url":"http://peer-a"}`); got != 204 {
		t.Errorf("heartbeat status = %d, want 204", got)
	}

	if got := post("/unregister", `{"url":"http://peer-a"}`); got != 204 {
		t.Errorf("unregister status = %d, want 204", got)
	}
	urls = listURLs()
	if len(urls) != 1 || urls[0] != "http://peer-b" {
		t.Errorf("after unregister got %v, want [http://peer-b]", urls)
	}

	if got := post("/register", `{}`); got != 400 {
		t.Errorf("empty url status = %d, want 400", got)
	}
}

func TestRegistryListReturnsSnapshot(t *testing.T) {
	reg := New(nopLogger())
	reg.Register("http://x")
	reg.Register("http://y")
	snap := reg.List()
	// Mutating via Register after the snapshot must not mutate snap.
	reg.Register("http://z")
	if len(snap) != 2 {
		t.Errorf("snapshot grew unexpectedly: %d", len(snap))
	}
}

// TestHealthzReturnsOK is a one-liner sanity check used by `a2abridge
// doctor` to detect a live directory.
func TestHealthzReturnsOK(t *testing.T) {
	reg := New(nopLogger())
	ts := httptest.NewServer(reg.Routes())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := bytes.NewBuffer(nil)
	_, _ = body.ReadFrom(resp.Body)
	if got := body.String(); got != "ok" {
		t.Errorf("healthz body = %q, want ok", got)
	}
}
