package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

func getCard(t *testing.T, url, ifNoneMatch string) rawResponse {
	t.Helper()
	return fetchCard(t, http.MethodGet, url, ifNoneMatch)
}

func fetchCard(t *testing.T, method, url, ifNoneMatch string) rawResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url+a2asrv.WellKnownAgentCardPath, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET card: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rawResponse{status: resp.StatusCode, header: resp.Header, body: body}
}

// TestAgentCardCachingHeaders: CARD-CACHE-001/002/003 plus conditional GET.
func TestAgentCardCachingHeaders(t *testing.T) {
	a := newTestAgent(t, "peer")

	first := getCard(t, a.url, "")
	if first.status != http.StatusOK || len(first.body) == 0 {
		t.Fatalf("GET card = %d, %d bytes", first.status, len(first.body))
	}
	if cc := first.header.Get("Cache-Control"); !strings.Contains(cc, "max-age=300") {
		t.Errorf("Cache-Control = %q, want max-age=300", cc)
	}
	etag := first.header.Get("ETag")
	if len(etag) < 3 || !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("ETag = %q, want a quoted strong tag", etag)
	}
	if _, err := time.Parse(http.TimeFormat, first.header.Get("Last-Modified")); err != nil {
		t.Errorf("Last-Modified = %q: %v", first.header.Get("Last-Modified"), err)
	}

	if again := getCard(t, a.url, ""); again.header.Get("ETag") != etag {
		t.Errorf("ETag changed between requests: %q vs %q", etag, again.header.Get("ETag"))
	}

	for _, match := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
		notModified := getCard(t, a.url, match)
		if notModified.status != http.StatusNotModified || len(notModified.body) != 0 {
			t.Errorf("If-None-Match %q = %d with %d bytes, want 304 empty", match, notModified.status, len(notModified.body))
		}
		if notModified.header.Get("ETag") != etag {
			t.Errorf("304 ETag = %q, want %q", notModified.header.Get("ETag"), etag)
		}
		if notModified.header.Get("Cache-Control") == "" || notModified.header.Get("Last-Modified") == "" {
			t.Errorf("304 lacks Cache-Control or Last-Modified: %v", notModified.header)
		}
	}

	if stale := getCard(t, a.url, `"stale"`); stale.status != http.StatusOK || len(stale.body) == 0 {
		t.Errorf("stale If-None-Match = %d with %d bytes, want 200 with card", stale.status, len(stale.body))
	}
}

// TestAgentCardHeadAndPreflight: HEAD gets the GET headers without a body,
// including the 304 path; a CORS preflight still reaches a2a-go's handler.
func TestAgentCardHeadAndPreflight(t *testing.T) {
	a := newTestAgent(t, "peer")
	etag := getCard(t, a.url, "").header.Get("ETag")

	head := fetchCard(t, http.MethodHead, a.url, "")
	if head.status != http.StatusOK || len(head.body) != 0 {
		t.Fatalf("HEAD card = %d with %d bytes, want 200 empty", head.status, len(head.body))
	}
	for _, name := range []string{"Cache-Control", "ETag", "Last-Modified", "Content-Type"} {
		if head.header.Get(name) == "" {
			t.Errorf("HEAD response lacks %s", name)
		}
	}
	if head.header.Get("ETag") != etag {
		t.Errorf("HEAD ETag = %q, want %q", head.header.Get("ETag"), etag)
	}

	headNotModified := fetchCard(t, http.MethodHead, a.url, etag)
	if headNotModified.status != http.StatusNotModified || headNotModified.header.Get("Cache-Control") == "" {
		t.Errorf("conditional HEAD = %d, headers %v; want 304 with cache headers", headNotModified.status, headNotModified.header)
	}

	preflight := fetchCard(t, http.MethodOptions, a.url, "")
	if preflight.status != http.StatusOK || preflight.header.Get("Access-Control-Allow-Methods") == "" {
		t.Errorf("OPTIONS card = %d, headers %v; want a2a-go preflight response", preflight.status, preflight.header)
	}
	if preflight.header.Get("ETag") != "" {
		t.Errorf("OPTIONS response carries cache headers: %v", preflight.header)
	}
}
