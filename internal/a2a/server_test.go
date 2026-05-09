package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeHandler is a controllable Handler used by tests. Each method
// returns whatever was wired up via the Set* fields. Default values are
// happy-path stubs.
type fakeHandler struct {
	sendTask    *Task
	sendMessage *Message
	sendErr     error
	getTask     *Task
	getErr      error
	cancelTask  *Task
	cancelErr   error
	listTasks   []Task
	listErr     error
	streamSend  func(context.Context, MessageSendParams, chan<- StreamResponse) error
	subscribe   func(context.Context, string, chan<- StreamResponse) error
}

func (f *fakeHandler) SendMessage(_ context.Context, _ MessageSendParams) (*Task, *Message, error) {
	return f.sendTask, f.sendMessage, f.sendErr
}
func (f *fakeHandler) GetTask(_ context.Context, _ TaskIDParams) (*Task, error) {
	return f.getTask, f.getErr
}
func (f *fakeHandler) CancelTask(_ context.Context, _ TaskIDParams) (*Task, error) {
	return f.cancelTask, f.cancelErr
}
func (f *fakeHandler) ListTasks(_ context.Context) ([]Task, error) {
	return f.listTasks, f.listErr
}
func (f *fakeHandler) Subscribe(ctx context.Context, id string, ch chan<- StreamResponse) error {
	if f.subscribe != nil {
		return f.subscribe(ctx, id, ch)
	}
	return nil
}
func (f *fakeHandler) StreamSend(ctx context.Context, p MessageSendParams, ch chan<- StreamResponse) error {
	if f.streamSend != nil {
		return f.streamSend(ctx, p, ch)
	}
	return nil
}

// nopLogger discards everything; tests don't need log output.
func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T, h Handler) *httptest.Server {
	t.Helper()
	srv := &Server{
		Card: AgentCard{
			ProtocolVersion:    ProtocolVersion,
			Name:               "test-agent",
			URL:                "http://test",
			PreferredTransport: "JSONRPC",
			Version:            "0.0.0",
		},
		Handler: h,
		Log:     nopLogger(),
	}
	return httptest.NewServer(srv.Routes())
}

func TestAgentCardEndpoint(t *testing.T) {
	ts := newTestServer(t, &fakeHandler{})
	defer ts.Close()

	resp, err := http.Get(ts.URL + WellKnownPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatal(err)
	}
	if card.Name != "test-agent" {
		t.Errorf("card.Name = %q, want test-agent", card.Name)
	}
	if card.ProtocolVersion != ProtocolVersion {
		t.Errorf("card.ProtocolVersion = %q, want %q", card.ProtocolVersion, ProtocolVersion)
	}
}

func TestRPCContractTable(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		handler   *fakeHandler
		wantCode  int    // expected JSON-RPC error code (0 = no error)
		wantField string // a JSON path that must appear in the response (substring match)
	}{
		{
			name:      "send message returns task",
			body:      `{"jsonrpc":"2.0","id":1,"method":"a2a.SendMessage","params":{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"hi"}]}}}`,
			handler:   &fakeHandler{sendTask: &Task{ID: "t1", Status: TaskStatus{State: TaskStateSubmitted}}},
			wantCode:  0,
			wantField: `"id":"t1"`,
		},
		{
			name:      "get task happy path",
			body:      `{"jsonrpc":"2.0","id":2,"method":"a2a.GetTask","params":{"id":"t1"}}`,
			handler:   &fakeHandler{getTask: &Task{ID: "t1", Status: TaskStatus{State: TaskStateCompleted}}},
			wantCode:  0,
			wantField: `"state":"TASK_STATE_COMPLETED"`,
		},
		{
			name:      "get task not found",
			body:      `{"jsonrpc":"2.0","id":3,"method":"a2a.GetTask","params":{"id":"missing"}}`,
			handler:   &fakeHandler{getErr: ErrTaskNotFound},
			wantCode:  ErrCodeTaskNotFound,
			wantField: `"code":-32001`,
		},
		{
			name:      "list tasks",
			body:      `{"jsonrpc":"2.0","id":4,"method":"a2a.ListTasks"}`,
			handler:   &fakeHandler{listTasks: []Task{{ID: "a"}, {ID: "b"}}},
			wantCode:  0,
			wantField: `"id":"a"`,
		},
		{
			name:      "cancel task",
			body:      `{"jsonrpc":"2.0","id":5,"method":"a2a.CancelTask","params":{"id":"t1"}}`,
			handler:   &fakeHandler{cancelTask: &Task{ID: "t1", Status: TaskStatus{State: TaskStateCanceled}}},
			wantCode:  0,
			wantField: `"state":"TASK_STATE_CANCELED"`,
		},
		{
			name:      "extended agent card",
			body:      `{"jsonrpc":"2.0","id":6,"method":"a2a.GetExtendedAgentCard"}`,
			handler:   &fakeHandler{},
			wantCode:  0,
			wantField: `"name":"test-agent"`,
		},
		{
			name:      "unknown method",
			body:      `{"jsonrpc":"2.0","id":7,"method":"a2a.NoSuchMethod"}`,
			handler:   &fakeHandler{},
			wantCode:  ErrCodeMethodNotFound,
			wantField: `"code":-32601`,
		},
		{
			name:      "wrong jsonrpc version",
			body:      `{"jsonrpc":"1.0","id":8,"method":"a2a.ListTasks"}`,
			handler:   &fakeHandler{},
			wantCode:  ErrCodeInvalidRequest,
			wantField: `"code":-32600`,
		},
		{
			name:      "malformed json",
			body:      `not json`,
			handler:   &fakeHandler{},
			wantCode:  ErrCodeParse,
			wantField: `"code":-32700`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, tc.handler)
			defer ts.Close()
			resp, err := http.Post(ts.URL+"/", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tc.wantField) {
				t.Errorf("response missing %q\n  got: %s", tc.wantField, body)
			}
		})
	}
}

func TestStreamingSendEmitsTaskAndStatusUpdate(t *testing.T) {
	h := &fakeHandler{
		streamSend: func(_ context.Context, _ MessageSendParams, ch chan<- StreamResponse) error {
			ch <- StreamResponse{Task: &Task{ID: "stream-t1", Status: TaskStatus{State: TaskStateWorking}}}
			ch <- StreamResponse{StatusUpdate: &TaskStatusUpdateEvent{TaskID: "stream-t1", Status: TaskStatus{State: TaskStateCompleted}}}
			return nil
		},
	}
	ts := newTestServer(t, h)
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":99,"method":"a2a.SendStreamingMessage","params":{"message":{"messageId":"x","role":"ROLE_USER","parts":[{"text":"go"}]}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/", strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Drain SSE for ~200ms — both events should land within that window.
	buf := bytes.NewBuffer(nil)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(buf, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
	}
	out := buf.String()
	if !strings.Contains(out, `"state":"TASK_STATE_WORKING"`) {
		t.Errorf("missing WORKING event:\n%s", out)
	}
	if !strings.Contains(out, `"state":"TASK_STATE_COMPLETED"`) {
		t.Errorf("missing COMPLETED event:\n%s", out)
	}
}
