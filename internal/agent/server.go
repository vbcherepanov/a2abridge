package agent

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/vbcherepanov/a2abridge/v4/internal/metrics"
)

// TextMediaType is the only input/output mode a bridge speaks.
const TextMediaType = "text/plain"

// maxRequestBodyBytes bounds every request body. A federated bridge listens
// on the network, and neither a2a-go nor net/http limits bodies by default.
const maxRequestBodyBytes = 10 << 20

// NewA2AHTTPHandler assembles the A2A 1.0 HTTP surface of an agent:
//
//	GET  /.well-known/agent-card.json   Agent Card
//	POST /                              JSON-RPC binding
//	/message:send, /message:stream,
//	/tasks, /tasks/..., /extendedAgentCard  HTTP+JSON binding
//	GET  /healthz, GET /metrics         operational endpoints
//
// The card's capabilities drive a2a-go's capability checks. Around the SDK
// handlers the agent caps request bodies at maxRequestBodyBytes, validates
// A2A-Version, rejects SubscribeToTask for missing or finished tasks with a
// plain error, serves the card with cache headers and stamps event
// timestamps in UTC.
func NewA2AHTTPHandler(
	card *a2a.AgentCard,
	executor a2asrv.AgentExecutor,
	tasks taskstore.Store,
	pushConfigs push.ConfigStore,
	pushSender push.Sender,
	log *slog.Logger,
) (http.Handler, error) {
	cardHandler, err := newAgentCardHandler(card, time.Now())
	if err != nil {
		return nil, err
	}
	handler := a2asrv.NewHandler(utcExecutor{next: executor},
		a2asrv.WithTaskStore(tasks),
		a2asrv.WithPushNotifications(pushConfigs, pushSender),
		a2asrv.WithCapabilityChecks(&card.Capabilities),
		a2asrv.WithLogger(log),
	)
	guard := &protocolGuard{tasks: tasks, streaming: card.Capabilities.Streaming, log: log}
	rest := guard.rest(a2asrv.NewRESTHandler(handler))

	mux := http.NewServeMux()
	// No method in the pattern: the card handler answers CORS preflight itself.
	mux.Handle(a2asrv.WellKnownAgentCardPath, cardHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte("ok")); err != nil {
			log.Debug("healthz write failed", "err", err)
		}
	})
	// Per-bridge metrics (inbox_size, messages_received, tasks_failed, ...).
	mux.Handle("GET /metrics", metrics.Handler())
	// {$} pins the JSON-RPC endpoint to exactly "/" so unknown paths get a
	// proper 404 from the mux instead of falling into the dispatcher.
	mux.Handle("POST /{$}", guard.jsonrpc(a2asrv.NewJSONRPCHandler(handler)))
	// The REST handler routes methods itself; only its paths are delegated.
	for _, pattern := range []string{"/message:send", "/message:stream", "/tasks", "/tasks/", "/extendedAgentCard"} {
		mux.Handle(pattern, rest)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		mux.ServeHTTP(w, r)
	}), nil
}
