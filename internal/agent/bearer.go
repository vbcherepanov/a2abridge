package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// Bearer-token auth for peers that cannot do mTLS (Hermes and other A2A
// agents using the spec's httpAuth scheme).
//
//	A2A_PEER_TOKENS   outbound, peer=token pairs; peer is a base URL or a card name
//	A2A_AUTH_TOKENS   inbound, name:token pairs (the format Hermes uses)
//
// Each also has a _FILE variant holding the same pairs. Tokens never reach a
// log, an error message or a tool result.

// bearerSchemeName names the scheme in the Agent Card.
const bearerSchemeName a2a.SecuritySchemeName = "bearer"

// PeerTokens maps outbound peers to bearer tokens.
type PeerTokens struct {
	byURL  []urlToken
	byName map[string]string
}

type urlToken struct {
	origin string // scheme://host:port, lower case
	path   string // path prefix without a trailing slash
	token  string
}

// AuthTokens is the inbound name:token list.
type AuthTokens struct {
	entries []authEntry
}

type authEntry struct {
	name   string
	digest [sha256.Size]byte
}

// LoadPeerTokens reads A2A_PEER_TOKENS or A2A_PEER_TOKENS_FILE. It returns
// nil when neither is set.
func LoadPeerTokens() (*PeerTokens, error) {
	const envName = "A2A_PEER_TOKENS"
	entries, err := readTokenEntries(envName)
	if err != nil || entries == nil {
		return nil, err
	}
	pt := &PeerTokens{byName: map[string]string{}}
	for i, e := range entries {
		key, tok, ok := strings.Cut(e, "=")
		key, tok = strings.TrimSpace(key), strings.TrimSpace(tok)
		if !ok || key == "" || tok == "" {
			return nil, fmt.Errorf("%s entry %d: want peer=token, with a base URL or an agent name as peer", envName, i+1)
		}
		if !strings.Contains(key, "://") {
			pt.byName[key] = tok
			continue
		}
		u, err := url.Parse(key)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("%s entry %d: peer is not an http(s) base URL", envName, i+1)
		}
		if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("%s entry %d: refusing to send a token over plain HTTP to %s, which is not loopback; use an https:// URL", envName, i+1, u.Host)
		}
		pt.byURL = append(pt.byURL, urlToken{origin: originOf(u), path: strings.TrimRight(u.Path, "/"), token: tok})
	}
	return pt, nil
}

// LoadAuthTokens reads A2A_AUTH_TOKENS or A2A_AUTH_TOKENS_FILE. It returns
// nil when neither is set.
func LoadAuthTokens() (*AuthTokens, error) {
	const envName = "A2A_AUTH_TOKENS"
	entries, err := readTokenEntries(envName)
	if err != nil || entries == nil {
		return nil, err
	}
	at := &AuthTokens{}
	for i, e := range entries {
		name, tok, ok := strings.Cut(e, ":")
		name, tok = strings.TrimSpace(name), strings.TrimSpace(tok)
		if !ok || name == "" || tok == "" {
			return nil, fmt.Errorf("%s entry %d: want name:token", envName, i+1)
		}
		at.entries = append(at.entries, authEntry{name: name, digest: sha256.Sum256([]byte(tok))})
	}
	return at, nil
}

// readTokenEntries returns the non-empty entries of envName or of the file
// named by envName_FILE: pairs split on commas and newlines, # comments and
// blank lines skipped. nil, nil means neither is set.
func readTokenEntries(envName string) ([]string, error) {
	fileEnv := envName + "_FILE"
	raw, path := os.Getenv(envName), os.Getenv(fileEnv)
	source := envName
	switch {
	case raw != "" && path != "":
		return nil, fmt.Errorf("both %s and %s are set; set one of them", envName, fileEnv)
	case path != "":
		b, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own configuration
		if err != nil {
			// The path is not secret, but the error from a file holding
			// tokens never quotes its content, only that it is unreadable.
			return nil, fmt.Errorf("%s: cannot read %s: %w", fileEnv, path, errors.Unwrap(err))
		}
		raw, source = string(b), fileEnv
	case raw == "":
		return nil, nil
	}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, e := range strings.Split(line, ",") {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
	}
	if out == nil {
		return nil, fmt.Errorf("%s holds no entries", source)
	}
	return out, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// originOf is scheme://host:port with the default port spelled out, so
// https://x and https://x:443 compare equal.
func originOf(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(u.Scheme)]
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// forURL returns the URL-keyed token for u, longest path prefix first.
func (p *PeerTokens) forURL(u *url.URL) (string, bool) {
	if p == nil {
		return "", false
	}
	origin, best, tok := originOf(u), -1, ""
	for _, e := range p.byURL {
		if e.origin != origin || len(e.path) <= best {
			continue
		}
		if e.path == "" || u.Path == e.path || strings.HasPrefix(u.Path, e.path+"/") {
			best, tok = len(e.path), e.token
		}
	}
	return tok, best >= 0
}

// forName returns the token keyed by an agent name.
func (p *PeerTokens) forName(name string) (string, bool) {
	if p == nil || name == "" {
		return "", false
	}
	tok, ok := p.byName[name]
	return tok, ok
}

// bearerTransport adds Authorization: Bearer to requests bound for the peer a
// token is configured for, and to no one else. It works per request, on the
// request's own URL, so a redirect to another origin carries no token.
type bearerTransport struct {
	next   http.RoundTripper
	tokens *PeerTokens
	// nameOrigin/nameToken bind a name-keyed token to the origin of the
	// peer_url whose card carried that name. Empty when unbound.
	nameOrigin, nameToken string
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, ok := t.tokens.forURL(r.URL)
	if !ok && t.nameOrigin != "" && originOf(r.URL) == t.nameOrigin {
		tok, ok = t.nameToken, true
	}
	if !ok {
		return t.next.RoundTrip(r)
	}
	if !strings.EqualFold(r.URL.Scheme, "https") && !isLoopbackHost(r.URL.Hostname()) {
		if r.Body != nil {
			_ = r.Body.Close() // RoundTrip must close the body, even on error
		}
		return nil, fmt.Errorf("refusing to send a bearer token over plain HTTP to %s, which is not loopback; use an https:// peer URL", r.URL.Host)
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return t.next.RoundTrip(r)
}

// authNameKey carries the authenticated token name from the HTTP middleware
// to the A2A call interceptor.
type authNameKey struct{}

// requireBearer answers 401 to a request without a configured token and
// passes the token's name on in the request context.
func (a *AuthTokens) requireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		name, ok := "", false
		if strings.EqualFold(scheme, "Bearer") {
			name, ok = a.match(strings.TrimSpace(tok))
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authNameKey{}, name)))
	})
}

// match compares tok against every configured token in constant time. It
// compares SHA-256 digests, so neither the token length nor which entry
// matched shows in the timing.
func (a *AuthTokens) match(tok string) (string, bool) {
	digest := sha256.Sum256([]byte(tok))
	name, found := "", 0
	for _, e := range a.entries {
		eq := subtle.ConstantTimeCompare(digest[:], e.digest[:])
		if eq == 1 && found == 0 {
			name = e.name
		}
		found |= eq
	}
	return name, found == 1
}

// authInterceptor copies the token name into the A2A call's User, which
// a2a-go hands to the executor. It keeps any attributes already set.
type authInterceptor struct {
	a2asrv.PassthroughCallInterceptor
}

func (authInterceptor) Before(ctx context.Context, callCtx *a2asrv.CallContext, _ *a2asrv.Request) (context.Context, any, error) {
	if name, ok := ctx.Value(authNameKey{}).(string); ok {
		if callCtx.User == nil {
			callCtx.User = &a2asrv.User{}
		}
		callCtx.User.Name = name
		callCtx.User.Authenticated = true
	}
	return ctx, nil, nil
}

// AdvertiseBearer declares the bearer scheme on the card and requires it.
func AdvertiseBearer(card *a2a.AgentCard) {
	card.SecuritySchemes = a2a.NamedSecuritySchemes{
		bearerSchemeName: a2a.HTTPAuthSecurityScheme{Scheme: "Bearer", Description: "a token from the agent's A2A_AUTH_TOKENS"},
	}
	card.SecurityRequirements = a2a.SecurityRequirementsOptions{{bearerSchemeName: {}}}
}
