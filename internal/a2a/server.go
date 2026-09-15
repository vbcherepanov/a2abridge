package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/metrics"
)

// Handler implements the business logic behind A2A RPC methods.
// Implementers decide how to process incoming messages, create tasks, etc.
type Handler interface {
	SendMessage(ctx context.Context, p MessageSendParams) (*Task, *Message, error)
	GetTask(ctx context.Context, p TaskIDParams) (*Task, error)
	CancelTask(ctx context.Context, p TaskIDParams) (*Task, error)
	ListTasks(ctx context.Context) ([]Task, error)
	// Subscribe streams events for an existing task until ctx done or task is terminal.
	Subscribe(ctx context.Context, id string, out chan<- StreamResponse) error
	// StreamSend handles message/stream and emits events as they happen.
	StreamSend(ctx context.Context, p MessageSendParams, out chan<- StreamResponse) error
}

// PushHandler is an OPTIONAL extension implemented by handlers that
// support webhook-based push notifications per A2A 1.0 §9.5. Bridges
// register peer-supplied webhooks; the bridge POSTs status updates to
// each registered URL when the underlying task state changes. Handlers
// that do not implement this surface return -32003 (PushNotificationNotSupported).
type PushHandler interface {
	CreatePushConfig(ctx context.Context, p TaskPushNotificationConfig) (*TaskPushNotificationConfig, error)
	GetPushConfig(ctx context.Context, p PushNotificationConfigParams) (*TaskPushNotificationConfig, error)
	ListPushConfigs(ctx context.Context, taskID string) ([]TaskPushNotificationConfig, error)
	DeletePushConfig(ctx context.Context, p PushNotificationConfigParams) error
}

// Server exposes an A2A-compliant HTTP endpoint.
type Server struct {
	Card    AgentCard
	Handler Handler
	Log     *slog.Logger
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+WellKnownPath, s.handleAgentCard)
	// Legacy card location served for pre-1.0 peers during rolling upgrades.
	mux.HandleFunc("GET "+WellKnownPathLegacy, s.handleAgentCard)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// Per-bridge metrics (inbox_size, messages_received, tasks_failed, ...).
	// The directory already mounts this; without it here a bot's /metrics 404s,
	// so Eir can't scrape per-bridge stats.
	mux.Handle("GET /metrics", metrics.Handler())
	// {$} pins the JSON-RPC endpoint to exactly "/" so mistyped REST paths
	// get a proper 404 from the mux instead of falling into the dispatcher.
	mux.HandleFunc("POST /{$}", s.handleRPC)
	// Non-spec convenience REST API — see internal/a2a/rest.go.
	s.restRoutes(mux)
	return mux
}

func (s *Server) handleAgentCard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Card)
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, nil, ErrCodeParse, "parse error")
		return
	}
	if req.JSONRPC != "2.0" {
		writeErr(w, req.ID, ErrCodeInvalidRequest, "jsonrpc must be 2.0")
		return
	}
	// Accept deprecated proto-style method names from pre-1.0 peers.
	if canonical, ok := legacyMethodAliases[req.Method]; ok {
		req.Method = canonical
	}

	switch req.Method {
	case MethodSendMessage:
		p, ok := decodeParams[MessageSendParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		task, msg, err := s.Handler.SendMessage(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		// Result is one-of Task | Message.
		switch {
		case task != nil:
			writeOK(w, req.ID, task)
		case msg != nil:
			writeOK(w, req.ID, msg)
		default:
			writeErr(w, req.ID, ErrCodeInternal, "handler returned no result")
		}

	case MethodGetTask:
		p, ok := decodeParams[TaskIDParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		task, err := s.Handler.GetTask(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, task)

	case MethodCancelTask:
		p, ok := decodeParams[TaskIDParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		task, err := s.Handler.CancelTask(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, task)

	case MethodListTasks:
		tasks, err := s.Handler.ListTasks(r.Context())
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, tasks)

	case MethodSendStreamingMessage:
		p, ok := decodeParams[MessageSendParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		s.streamRPC(w, r, req.ID, func(ctx context.Context, ch chan<- StreamResponse) error {
			return s.Handler.StreamSend(ctx, p, ch)
		})

	case MethodSubscribeToTask:
		p, ok := decodeParams[TaskIDParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		s.streamRPC(w, r, req.ID, func(ctx context.Context, ch chan<- StreamResponse) error {
			return s.Handler.Subscribe(ctx, p.ID, ch)
		})

	case MethodGetExtendedCard:
		if !s.Card.Capabilities.ExtendedAgentCard {
			writeErr(w, req.ID, ErrCodeUnsupportedOperation, "extended agent card not supported")
			return
		}
		writeOK(w, req.ID, s.Card)

	case MethodCreatePushConfig:
		ph, ok := s.pushHandler(w, req.ID)
		if !ok {
			return
		}
		p, ok := decodeParams[TaskPushNotificationConfig](w, req.ID, req.Params)
		if !ok {
			return
		}
		out, err := ph.CreatePushConfig(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, out)

	case MethodGetPushConfig:
		ph, ok := s.pushHandler(w, req.ID)
		if !ok {
			return
		}
		p, ok := decodeParams[PushNotificationConfigParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		out, err := ph.GetPushConfig(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, out)

	case MethodListPushConfig:
		ph, ok := s.pushHandler(w, req.ID)
		if !ok {
			return
		}
		p, ok := decodeParams[PushNotificationConfigParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		out, err := ph.ListPushConfigs(r.Context(), p.TaskID)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, out)

	case MethodDeletePushConfig:
		ph, ok := s.pushHandler(w, req.ID)
		if !ok {
			return
		}
		p, ok := decodeParams[PushNotificationConfigParams](w, req.ID, req.Params)
		if !ok {
			return
		}
		if err := ph.DeletePushConfig(r.Context(), p); err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, map[string]any{"ok": true})

	default:
		writeErr(w, req.ID, ErrCodeMethodNotFound, "unknown method: "+req.Method)
	}
}

// decodeParams unmarshals raw JSON-RPC params into T; on failure it writes
// an InvalidParams error response and reports false.
func decodeParams[T any](w http.ResponseWriter, id, raw json.RawMessage) (T, bool) {
	var p T
	if err := json.Unmarshal(raw, &p); err != nil {
		writeErr(w, id, ErrCodeInvalidParams, err.Error())
		return p, false
	}
	return p, true
}

// pushHandler type-asserts the optional PushHandler surface; on failure it
// writes the spec -32003 error response and reports false.
func (s *Server) pushHandler(w http.ResponseWriter, id json.RawMessage) (PushHandler, bool) {
	ph, ok := s.Handler.(PushHandler)
	if !ok {
		writeErr(w, id, ErrCodePushNotificationNotSupported, "push notifications not supported by this agent")
	}
	return ph, ok
}

func (s *Server) streamRPC(
	w http.ResponseWriter, r *http.Request, id json.RawMessage,
	run func(ctx context.Context, ch chan<- StreamResponse) error,
) {
	s.streamSSE(w, r, run,
		func(ev StreamResponse) []byte {
			b, _ := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: ev})
			return b
		},
		func(err error) []byte {
			b, _ := json.Marshal(JSONRPCResponse{
				JSONRPC: "2.0", ID: id,
				Error: &JSONRPCError{Code: taskErrCode(err), Message: err.Error()},
			})
			return b
		},
		func(w http.ResponseWriter, err error) {
			writeErr(w, id, taskErrCode(err), err.Error())
		},
	)
}

// streamSSE drives an SSE response for both the JSON-RPC and REST stream
// endpoints. The handler runs in a goroutine; each event is encoded with
// encodeEvent. The SSE headers are written lazily: if the handler fails
// before the first event the error is surfaced as a regular (non-SSE)
// response via writePreErr; if it fails mid-stream a final SSE event
// produced by encodeErr is emitted.
func (s *Server) streamSSE(
	w http.ResponseWriter, r *http.Request,
	run func(ctx context.Context, ch chan<- StreamResponse) error,
	encodeEvent func(StreamResponse) []byte,
	encodeErr func(error) []byte,
	writePreErr func(http.ResponseWriter, error),
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writePreErr(w, errors.New("streaming unsupported"))
		return
	}

	ch := make(chan StreamResponse, 8)
	errc := make(chan error, 1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		errc <- run(ctx, ch)
		close(ch)
	}()
	// Drain leftovers after exit so handlers doing bare sends on the
	// buffered channel never block forever (goroutine leak).
	defer func() {
		go func() {
			for range ch {
			}
		}()
	}()

	streaming := false
	startStream := func() {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		streaming = true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				err := <-errc
				if err == nil || errors.Is(err, context.Canceled) {
					if !streaming {
						startStream() // empty-but-successful stream
					}
					return
				}
				s.Log.Warn("stream handler error", "err", err)
				if !streaming {
					writePreErr(w, err)
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", encodeErr(err))
				flusher.Flush()
				return
			}
			if !streaming {
				startStream()
			}
			fmt.Fprintf(w, "data: %s\n\n", encodeEvent(ev))
			flusher.Flush()
		case <-time.After(15 * time.Second):
			if !streaming {
				startStream()
			}
			// keepalive comment
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func writeOK(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeErr(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(JSONRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &JSONRPCError{Code: code, Message: msg},
	})
}

// Sentinel errors handlers return to signal spec error codes; both the
// JSON-RPC dispatcher (taskErrCode) and the REST layer (writeRESTErr) map
// them with errors.Is, so wrapping with fmt.Errorf("%w: ...") works.
var (
	// ErrTaskNotFound → -32001 / HTTP 404.
	ErrTaskNotFound = errors.New("task not found")
	// ErrTaskNotCancelable → -32002 / HTTP 409.
	ErrTaskNotCancelable = errors.New("task cannot be canceled")
	// ErrPushNotSupported → -32003 / HTTP 501.
	ErrPushNotSupported = errors.New("push notifications not supported")
	// ErrUnsupportedOperation → -32004 / HTTP 501.
	ErrUnsupportedOperation = errors.New("operation not supported")
	// ErrInvalidParams → -32602 / HTTP 400.
	ErrInvalidParams = errors.New("invalid params")
)

func taskErrCode(err error) int {
	switch {
	case errors.Is(err, ErrTaskNotFound):
		return ErrCodeTaskNotFound
	case errors.Is(err, ErrTaskNotCancelable):
		return ErrCodeTaskNotCancelable
	case errors.Is(err, ErrPushNotSupported):
		return ErrCodePushNotificationNotSupported
	case errors.Is(err, ErrUnsupportedOperation):
		return ErrCodeUnsupportedOperation
	case errors.Is(err, ErrInvalidParams):
		return ErrCodeInvalidParams
	default:
		return ErrCodeInternal
	}
}
