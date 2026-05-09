package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// REST endpoints per A2A 1.0 §7.3 (HTTP+REST binding). Mounted alongside
// the JSON-RPC POST / route, they let clients that don't speak JSON-RPC
// (cURL scripts, browser fetch(), webhook callers) consume the same
// handler. The bodies are direct JSON of the underlying types — no RPC
// envelope.
//
// Path table:
//
//	POST   /v1/tasks                   → SendMessage
//	POST   /v1/tasks/stream            → SendStreamingMessage (SSE)
//	GET    /v1/tasks                   → ListTasks
//	GET    /v1/tasks/{id}              → GetTask
//	POST   /v1/tasks/{id}/cancel       → CancelTask
//	GET    /v1/tasks/{id}/stream       → SubscribeToTask  (SSE)
//	GET    /v1/agent                   → GetExtendedAgentCard
//	POST   /v1/tasks/{id}/push         → CreatePushConfig
//	GET    /v1/tasks/{id}/push         → ListPushConfigs
//	DELETE /v1/tasks/{id}/push         → DeletePushConfig (all)
//	DELETE /v1/tasks/{id}/push/{cfg}   → DeletePushConfig (one)

func (s *Server) restRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/tasks", s.restSendMessage)
	mux.HandleFunc("POST /v1/tasks/stream", s.restSendStreaming)
	mux.HandleFunc("GET /v1/tasks", s.restListTasks)
	mux.HandleFunc("GET /v1/tasks/{id}", s.restGetTask)
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.restCancelTask)
	mux.HandleFunc("GET /v1/tasks/{id}/stream", s.restSubscribeToTask)
	mux.HandleFunc("GET /v1/agent", s.restAgentCard)
	mux.HandleFunc("POST /v1/tasks/{id}/push", s.restCreatePush)
	mux.HandleFunc("GET /v1/tasks/{id}/push", s.restListPush)
	mux.HandleFunc("DELETE /v1/tasks/{id}/push", s.restDeletePushAll)
	mux.HandleFunc("DELETE /v1/tasks/{id}/push/{cfg}", s.restDeletePushOne)
}

// --- helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeRESTErr maps internal handler errors to HTTP status codes per the
// A2A spec mapping conventions (§7.3, §8). TaskNotFound → 404, validation
// → 400, push-not-supported → 501, anything else → 500.
func writeRESTErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrTaskNotFound):
		status = http.StatusNotFound
	case strings.Contains(err.Error(), "required"):
		status = http.StatusBadRequest
	case strings.Contains(err.Error(), "not supported"):
		status = http.StatusNotImplemented
	}
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func (s *Server) restAgentCard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Card)
}

func (s *Server) restSendMessage(w http.ResponseWriter, r *http.Request) {
	var p MessageSendParams
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeRESTErr(w, fmt.Errorf("body required: %v", err))
		return
	}
	task, msg, err := s.Handler.SendMessage(r.Context(), p)
	if err != nil {
		writeRESTErr(w, err)
		return
	}
	if task != nil {
		writeJSON(w, http.StatusOK, task)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

func (s *Server) restListTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.Handler.ListTasks(r.Context())
	if err != nil {
		writeRESTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

func (s *Server) restGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Handler.GetTask(r.Context(), TaskIDParams{ID: id})
	if err != nil {
		writeRESTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) restCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Handler.CancelTask(r.Context(), TaskIDParams{ID: id})
	if err != nil {
		writeRESTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) restSendStreaming(w http.ResponseWriter, r *http.Request) {
	var p MessageSendParams
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeRESTErr(w, err)
		return
	}
	s.streamRESTRPC(w, r, func(ctx context.Context, ch chan<- StreamResponse) error {
		return s.Handler.StreamSend(ctx, p, ch)
	})
}

func (s *Server) restSubscribeToTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.streamRESTRPC(w, r, func(ctx context.Context, ch chan<- StreamResponse) error {
		return s.Handler.Subscribe(ctx, id, ch)
	})
}

// streamRESTRPC is the REST equivalent of streamRPC — same SSE framing,
// just without the JSON-RPC envelope around each event.
func (s *Server) streamRESTRPC(
	w http.ResponseWriter, r *http.Request,
	run func(ctx context.Context, ch chan<- StreamResponse) error,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeRESTErr(w, errors.New("streaming unsupported"))
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
			s.Log.Warn("rest stream handler error", "err", err)
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
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-time.After(15 * time.Second):
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) restCreatePush(w http.ResponseWriter, r *http.Request) {
	ph, ok := s.Handler.(PushHandler)
	if !ok {
		writeRESTErr(w, errors.New("push notifications not supported"))
		return
	}
	var cfg PushNotificationConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeRESTErr(w, err)
		return
	}
	out, err := ph.CreatePushConfig(r.Context(), TaskPushNotificationConfig{
		TaskID: r.PathValue("id"), Config: cfg,
	})
	if err != nil {
		writeRESTErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) restListPush(w http.ResponseWriter, r *http.Request) {
	ph, ok := s.Handler.(PushHandler)
	if !ok {
		writeRESTErr(w, errors.New("push notifications not supported"))
		return
	}
	out, err := ph.ListPushConfigs(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRESTErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) restDeletePushAll(w http.ResponseWriter, r *http.Request) {
	ph, ok := s.Handler.(PushHandler)
	if !ok {
		writeRESTErr(w, errors.New("push notifications not supported"))
		return
	}
	if err := ph.DeletePushConfig(r.Context(), PushNotificationConfigParams{TaskID: r.PathValue("id")}); err != nil {
		writeRESTErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restDeletePushOne(w http.ResponseWriter, r *http.Request) {
	ph, ok := s.Handler.(PushHandler)
	if !ok {
		writeRESTErr(w, errors.New("push notifications not supported"))
		return
	}
	if err := ph.DeletePushConfig(r.Context(), PushNotificationConfigParams{
		TaskID:       r.PathValue("id"),
		PushConfigID: r.PathValue("cfg"),
	}); err != nil {
		writeRESTErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
