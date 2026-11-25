package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// Peers opens A2A clients to peer bridges over one shared HTTP transport, so
// federation TLS and bearer tokens apply to card resolution and every
// protocol call alike.
type Peers struct {
	transport *http.Transport
	tokens    *PeerTokens
	http      *http.Client
	resolver  *agentcard.Resolver
	log       *slog.Logger
}

// NewPeers returns a peer client factory. tlsConfig is the federation client
// config (nil for plain loopback); tokens are the outbound bearer tokens (nil
// for none). The HTTP client has no overall timeout because streaming calls
// must stay open; callers bound calls with contexts.
func NewPeers(tlsConfig *tls.Config, tokens *PeerTokens, log *slog.Logger) *Peers {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	client := &http.Client{Transport: &bearerTransport{next: transport, tokens: tokens}}
	return &Peers{transport: transport, tokens: tokens, http: client, resolver: agentcard.NewResolver(client), log: log}
}

// Resolve fetches the peer's Agent Card from its well-known path.
func (p *Peers) Resolve(ctx context.Context, baseURL string) (*a2a.AgentCard, error) {
	card, err := p.resolver.Resolve(ctx, baseURL)
	if err != nil {
		return nil, fmt.Errorf("resolve agent card of %s: %w", baseURL, err)
	}
	return card, nil
}

// Client resolves the peer's card and connects over the interface it
// advertises (JSON-RPC preferred, HTTP+JSON otherwise). Callers must release
// the client with Release.
func (p *Peers) Client(ctx context.Context, baseURL string) (*a2aclient.Client, error) {
	card, err := p.Resolve(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	httpClient := p.http
	// A name-keyed token goes only to the origin of the URL we were asked to
	// call, and only when the card served there carries that name.
	if tok, ok := p.tokens.forName(card.Name); ok {
		u, err := url.Parse(baseURL)
		if err != nil {
			return nil, fmt.Errorf("peer url %s: %w", baseURL, err)
		}
		httpClient = &http.Client{Transport: &bearerTransport{
			next: p.transport, tokens: p.tokens,
			nameOrigin: originOf(u), nameToken: tok,
		}}
	}
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(httpClient),
		a2aclient.WithRESTTransport(httpClient),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", baseURL, err)
	}
	return client, nil
}

// GetTask fetches one task from a peer.
func (p *Peers) GetTask(ctx context.Context, baseURL string, id a2a.TaskID) (*a2a.Task, error) {
	client, err := p.Client(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	defer p.Release(client)
	task, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: id})
	if err != nil {
		return nil, fmt.Errorf("get task %s from %s: %w", id, baseURL, err)
	}
	return task, nil
}

// Release destroys a client obtained from Client.
func (p *Peers) Release(client *a2aclient.Client) {
	if err := client.Destroy(); err != nil {
		p.log.Warn("peer client destroy failed", "err", err)
	}
}
