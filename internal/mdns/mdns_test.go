package mdns

import (
	"net/url"
	"testing"

	"github.com/grandcat/zeroconf"
)

// We intentionally don't exercise the real Publish/Browse routines in
// unit tests — multicast UDP is unreliable in CI sandboxes and would
// flake. The tests below cover the pure-function helpers, which is
// where bugs actually hide.

func TestPortFromURLDefaults(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:8080", "8080"},
		{"http://127.0.0.1", "80"},
		{"https://peer.local", "443"},
		{"http://[::1]:42", "42"},
		// Empty scheme + no port → empty (caller will surface as Atoi error).
		{"//host", ""},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.in, err)
		}
		if got := portFromURL(u); got != tc.want {
			t.Errorf("portFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestURLFromEntryPrefersTXTRecord(t *testing.T) {
	// When a peer publishes "url=..." in TXT, that's authoritative —
	// even when HostName/Port disagree.
	e := &zeroconf.ServiceEntry{
		HostName: "wrong.local.",
		Port:     9999,
		Text:     []string{"a2a-version=1.0", "url=http://canonical:1234"},
	}
	if got := urlFromEntry(e); got != "http://canonical:1234" {
		t.Errorf("urlFromEntry = %q, want canonical", got)
	}
}

func TestURLFromEntryFallsBackToHostPort(t *testing.T) {
	e := &zeroconf.ServiceEntry{
		HostName: "peer.local.",
		Port:     7654,
		Text:     []string{"a2a-version=1.0"}, // no url= field
	}
	if got := urlFromEntry(e); got != "http://peer.local:7654" {
		t.Errorf("urlFromEntry = %q, want fallback", got)
	}
}

func TestURLFromEntryReturnsEmptyWhenIncomplete(t *testing.T) {
	if got := urlFromEntry(&zeroconf.ServiceEntry{}); got != "" {
		t.Errorf("expected empty for empty entry, got %q", got)
	}
}
