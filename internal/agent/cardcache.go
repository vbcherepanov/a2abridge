package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// agentCardMaxAge is how long clients may reuse the Agent Card (spec §8.6).
const agentCardMaxAge = 5 * time.Minute

// newAgentCardHandler serves the card through a2a-go's static handler and adds
// Cache-Control, a strong ETag over the served bytes and Last-Modified set to
// builtAt to GET and HEAD, answering a matching If-None-Match with 304. Other
// methods (CORS preflight) go to the static handler untouched.
func newAgentCardHandler(card *a2a.AgentCard, builtAt time.Time) (http.Handler, error) {
	// a2a-go's static handler serves exactly json.Marshal(card), so the ETag
	// is computed over the same bytes.
	body, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("encode agent card: %w", err)
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	lastModified := builtAt.UTC().Format(http.TimeFormat)
	cacheControl := "public, max-age=" + strconv.Itoa(int(agentCardMaxAge/time.Second))
	static := a2asrv.NewStaticAgentCardHandler(card)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			static.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", cacheControl)
		h.Set("ETag", etag)
		h.Set("Last-Modified", lastModified)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			// The static handler only answers GET; net/http drops the body
			// written for a HEAD request, leaving the same headers.
			get := r.Clone(r.Context())
			get.Method = http.MethodGet
			static.ServeHTTP(w, get)
			return
		}
		static.ServeHTTP(w, r)
	}), nil
}

// etagMatches implements the weak comparison If-None-Match uses (RFC 9110 §13.1.2).
func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}
