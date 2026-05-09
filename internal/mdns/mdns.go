// Package mdns publishes the local A2A peer over mDNS / DNS-SD so
// bridges on the same LAN can discover each other without a centralized
// directory. The service type is "_a2a._tcp.local.", per the A2A 1.0
// federation guidelines.
//
// Discovery is opt-in: the bridge only enables mDNS when the user sets
// A2A_MDNS=1 (or passes --mdns to `a2abridge bridge`). Disabled by default
// because broadcasting on every coffee-shop LAN isn't what most users
// want.
package mdns

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

const serviceType = "_a2a._tcp"

// Publisher advertises the local peer's URL on the LAN. Close() retracts
// the advertisement. A nil log is OK — we'll discard.
type Publisher struct {
	server *zeroconf.Server
	mu     sync.Mutex
}

// Publish registers the peer at the given URL with mDNS. instanceName is
// the per-bridge label (e.g. claude-ttys000). Port is parsed from the
// URL.
func Publish(instanceName, peerURL string, log *slog.Logger) (*Publisher, error) {
	u, err := url.Parse(peerURL)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portFromURL(u))
	if err != nil {
		return nil, err
	}
	if instanceName == "" {
		instanceName = "a2abridge-peer"
	}
	// Embed full URL in the TXT record so peers don't need to re-resolve
	// the host. mDNS hostnames sometimes lag.
	txt := []string{
		"a2a-version=1.0",
		"url=" + peerURL,
	}
	server, err := zeroconf.Register(instanceName, serviceType, "local.", port, txt, nil)
	if err != nil {
		return nil, err
	}
	if log != nil {
		log.Info("mdns published", "instance", instanceName, "type", serviceType, "port", port)
	}
	return &Publisher{server: server}, nil
}

// Close retracts the mDNS advertisement.
func (p *Publisher) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.server != nil {
		p.server.Shutdown()
		p.server = nil
	}
}

// Browse runs a single discovery sweep on the LAN, returning every peer
// URL advertised within `timeout`. Pass timeout=0 for the default 2s.
func Browse(ctx context.Context, timeout time.Duration) ([]string, error) {
	if timeout == 0 {
		timeout = 2 * time.Second
	}
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		return nil, err
	}
	entries := make(chan *zeroconf.ServiceEntry, 16)
	browseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := resolver.Browse(browseCtx, serviceType, "local.", entries); err != nil {
		return nil, err
	}

	var urls []string
	seen := map[string]bool{}
	for {
		select {
		case <-browseCtx.Done():
			return urls, nil
		case e, ok := <-entries:
			if !ok {
				return urls, nil
			}
			if u := urlFromEntry(e); u != "" && !seen[u] {
				seen[u] = true
				urls = append(urls, u)
			}
		}
	}
}

// urlFromEntry extracts the canonical URL from a ServiceEntry. We prefer
// the "url=" TXT field; if it's missing (e.g. a peer published with a
// different stack), we fall back to assembling http://<hostname>:<port>.
func urlFromEntry(e *zeroconf.ServiceEntry) string {
	for _, t := range e.Text {
		if strings.HasPrefix(t, "url=") {
			return strings.TrimPrefix(t, "url=")
		}
	}
	if e.HostName == "" || e.Port == 0 {
		return ""
	}
	host := strings.TrimSuffix(e.HostName, ".")
	return "http://" + host + ":" + strconv.Itoa(e.Port)
}

// portFromURL is a paranoid version of url.URL.Port that handles plain
// IPv4 / IPv6 hosts and refuses to return "" — callers downstream want
// an explicit error, not a confusing zero.
func portFromURL(u *url.URL) string {
	p := u.Port()
	if p == "" {
		switch u.Scheme {
		case "http":
			p = "80"
		case "https":
			p = "443"
		}
	}
	if p == "" {
		return "" // forces an Atoi error upstream
	}
	return p
}

// ErrDisabled — sentinel returned by helpers when the caller asked for
// mDNS but the env doesn't have it enabled. Lets callers `errors.Is` it
// without strings.
var ErrDisabled = errors.New("mdns disabled")
