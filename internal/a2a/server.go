package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
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
	// StreamSend handles a2a.SendStreamingMessage and emits events as they happen.
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /", s.handleRPC)
	// REST mirror per A2A 1.0 §7.3 — see internal/a2a/rest.go.
	s.restRoutes(mux)
	return mux
}

func (s *Server) handleAgentCard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Card)
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, nil, ErrCodeParse, "parse error")
		return
	}
	if req.JSONRPC != "2.0" {
		writeErr(w, req.ID, ErrCodeInvalidRequest, "jsonrpc must be 2.0")
		return
	}

	switch req.Method {
	case MethodSendMessage:
		var p MessageSendParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		task, msg, err := s.Handler.SendMessage(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, ErrCodeInternal, err.Error())
			return
		}
		// Result is one-of Task | Message.
		if task != nil {
			writeOK(w, req.ID, task)
		} else {
			writeOK(w, req.ID, msg)
		}

	case MethodGetTask:
		var p TaskIDParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		task, err := s.Handler.GetTask(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, task)

	case MethodCancelTask:
		var p TaskIDParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
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
			writeErr(w, req.ID, ErrCodeInternal, err.Error())
			return
		}
		writeOK(w, req.ID, tasks)

	case MethodSendStreamingMessage:
		var p MessageSendParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		s.streamRPC(w, r, req.ID, func(ctx context.Context, ch chan<- StreamResponse) error {
			return s.Handler.StreamSend(ctx, p, ch)
		})

	case MethodSubscribeToTask:
		var p TaskIDParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		s.streamRPC(w, r, req.ID, func(ctx context.Context, ch chan<- StreamResponse) error {
			return s.Handler.Subscribe(ctx, p.ID, ch)
		})

	case MethodGetExtendedCard:
		writeOK(w, req.ID, s.Card)

	case MethodCreatePushConfig:
		ph, ok := s.Handler.(PushHandler)
		if !ok {
			writeErr(w, req.ID, ErrCodePushNotificationNotSupported, "push notifications not supported by this agent")
			return
		}
		var p TaskPushNotificationConfig
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		out, err := ph.CreatePushConfig(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, out)

	case MethodGetPushConfig:
		ph, ok := s.Handler.(PushHandler)
		if !ok {
			writeErr(w, req.ID, ErrCodePushNotificationNotSupported, "push notifications not supported by this agent")
			return
		}
		var p PushNotificationConfigParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		out, err := ph.GetPushConfig(r.Context(), p)
		if err != nil {
			writeErr(w, req.ID, taskErrCode(err), err.Error())
			return
		}
		writeOK(w, req.ID, out)

	case MethodListPushConfig:
		ph, ok := s.Handler.(PushHandler)
		if !ok {
			writeErr(w, req.ID, ErrCodePushNotificationNotSupported, "push notifications not supported by this agent")
			return
		}
		var p PushNotificationConfigParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
			return
		}
		out, err := ph.ListPushConfigs(r.Context(), p.TaskID)
		if err != nil {
			writeErr(w, req.ID, ErrCodeInternal, err.Error())
			return
		}
		writeOK(w, req.ID, out)

	case MethodDeletePushConfig:
		ph, ok := s.Handler.(PushHandler)
		if !ok {
			writeErr(w, req.ID, ErrCodePushNotificationNotSupported, "push notifications not supported by this agent")
			return
		}
		var p PushNotificationConfigParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeErr(w, req.ID, ErrCodeInvalidParams, err.Error())
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

func (s *Server) streamRPC(
	w http.ResponseWriter, r *http.Request, id json.RawMessage,
	run func(ctx context.Context, ch chan<- StreamResponse) error,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, id, ErrCodeInternal, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := make(chan StreamResponse, 8)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go func() {
		defer close(ch)
		if err := run(ctx, ch); err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Warn("stream handler error", "err", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			resp := JSONRPCResponse{JSONRPC: "2.0", ID: id, Result: ev}
			b, _ := json.Marshal(resp)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-time.After(15 * time.Second):
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

// ErrTaskNotFound — handlers return this to signal 404 semantics.
var ErrTaskNotFound = errors.New("task not found")

func taskErrCode(err error) int {
	if errors.Is(err, ErrTaskNotFound) {
		return ErrCodeTaskNotFound
	}
	return ErrCodeInternal
}
