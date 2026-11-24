package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// agentCardMaxAge is how long clients may reuse the Agent Card (spec §8.6).
const agentCardMaxAge = 5 * time.Minute

// newAgentCardHandler serves the card through a2a-go's static handler, which
// sets Cache-Control and a weak ETag and answers a matching If-None-Match with
// 304. This wrapper adds Last-Modified set to builtAt and HEAD support. Other
// methods (CORS preflight) go to the static handler untouched.
func newAgentCardHandler(card *a2a.AgentCard, builtAt time.Time) (http.Handler, error) {
	// NewStaticAgentCardHandler panics when the card cannot be encoded.
	if _, err := json.Marshal(card); err != nil {
		return nil, fmt.Errorf("encode agent card: %w", err)
	}
	lastModified := builtAt.UTC().Format(http.TimeFormat)
	cacheControl := "public, max-age=" + strconv.Itoa(int(agentCardMaxAge/time.Second))
	static := a2asrv.NewStaticAgentCardHandler(card, a2asrv.WithAgentCardCacheControl(cacheControl))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			static.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Last-Modified", lastModified)
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
