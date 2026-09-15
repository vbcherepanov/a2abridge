package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

func rawCall(t *testing.T, method, url, version string, body []byte) rawResponse {
	t.Helper()
	var reader io.Reader = http.NoBody
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if version != "" {
		req.Header.Set(a2a.SvcParamVersion, version)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return rawResponse{status: resp.StatusCode, header: resp.Header, body: b}
}

func jsonrpcRequest(t *testing.T, id int, method string, params any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type rpcErrorProbe struct {
	ID    json.RawMessage `json:"id"`
	Error *struct {
		Code int              `json:"code"`
		Data []map[string]any `json:"data"`
	} `json:"error"`
}

type restErrorProbe struct {
	Error struct {
		Code    int              `json:"code"`
		Status  string           `json:"status"`
		Details []map[string]any `json:"details"`
	} `json:"error"`
}

// assertRPCError checks a plain (non-SSE) JSON-RPC error with the ErrorInfo detail.
func assertRPCError(t *testing.T, resp rawResponse, id string, code int, reason string) {
	t.Helper()
	if ct := resp.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json (plain error, not a stream); body %s", ct, resp.body)
	}
	var probe rpcErrorProbe
	if err := json.Unmarshal(resp.body, &probe); err != nil || probe.Error == nil {
		t.Fatalf("not a JSON-RPC error: %s (%v)", resp.body, err)
	}
	if string(probe.ID) != id || probe.Error.Code != code {
		t.Fatalf("id=%s code=%d, want id=%s code=%d; body %s", probe.ID, probe.Error.Code, id, code, resp.body)
	}
	if len(probe.Error.Data) != 1 || probe.Error.Data[0]["reason"] != reason || probe.Error.Data[0]["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" {
		t.Fatalf("error data = %v, want ErrorInfo %s", probe.Error.Data, reason)
	}
}

func assertRESTError(t *testing.T, resp rawResponse, status int, reason string) restErrorProbe {
	t.Helper()
	if resp.status != status {
		t.Fatalf("HTTP status = %d, want %d; body %s", resp.status, status, resp.body)
	}
	if ct := resp.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json; body %s", ct, resp.body)
	}
	var probe restErrorProbe
	if err := json.Unmarshal(resp.body, &probe); err != nil {
		t.Fatalf("not a REST error: %s (%v)", resp.body, err)
	}
	if probe.Error.Code != status || len(probe.Error.Details) != 1 || probe.Error.Details[0]["reason"] != reason {
		t.Fatalf("REST error = %+v, want code %d reason %s", probe.Error, status, reason)
	}
	return probe
}

// completedTaskID creates a task on the agent and completes it.
func completedTaskID(t *testing.T, a *testAgent) a2a.TaskID {
	t.Helper()
	c := a.client(t)
	task := sendImmediate(t, c, userMessage("finish me"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)
	if err := a.executor.CompleteTask(string(task.ID), "done"); err != nil {
		t.Fatal(err)
	}
	waitTaskState(t, c, task.ID, a2a.TaskStateCompleted)
	return task.ID
}

// TestSubscribeToMissingTaskIsPlainError: a2a-go would open an SSE stream
// carrying the error; clients must get TaskNotFound before any stream starts.
func TestSubscribeToMissingTaskIsPlainError(t *testing.T) {
	a := newTestAgent(t, "peer")

	resp := rawCall(t, http.MethodPost, a.url+"/", "1.0", jsonrpcRequest(t, 8, methodSubscribeToTask, map[string]any{"id": "nope"}))
	assertRPCError(t, resp, "8", -32001, "TASK_NOT_FOUND")

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		resp := rawCall(t, method, a.url+"/tasks/nope:subscribe", "1.0", nil)
		probe := assertRESTError(t, resp, http.StatusNotFound, "TASK_NOT_FOUND")
		if probe.Error.Status != "NOT_FOUND" {
			t.Errorf("%s status = %q, want NOT_FOUND", method, probe.Error.Status)
		}
		if meta, ok := probe.Error.Details[0]["metadata"].(map[string]any); !ok || meta["taskId"] != "nope" {
			t.Errorf("%s metadata = %v, want taskId nope", method, probe.Error.Details[0]["metadata"])
		}
	}
}

// TestSubscribeToTerminalTaskUnsupported: spec §3.1.6 — a finished task
// cannot be subscribed to.
func TestSubscribeToTerminalTaskUnsupported(t *testing.T) {
	a := newTestAgent(t, "peer")
	id := completedTaskID(t, a)

	resp := rawCall(t, http.MethodPost, a.url+"/", "1.0", jsonrpcRequest(t, 9, methodSubscribeToTask, map[string]any{"id": id}))
	assertRPCError(t, resp, "9", -32004, "UNSUPPORTED_OPERATION")

	rest := rawCall(t, http.MethodPost, a.url+"/tasks/"+string(id)+":subscribe", "1.0", nil)
	probe := assertRESTError(t, rest, http.StatusBadRequest, "UNSUPPORTED_OPERATION")
	if probe.Error.Status != "FAILED_PRECONDITION" {
		t.Errorf("status = %q, want FAILED_PRECONDITION", probe.Error.Status)
	}
}

// TestSubscribeToWorkingTaskStillStreams: the pre-check passes live tasks
// through to a2a-go, whose stream starts with the task snapshot.
func TestSubscribeToWorkingTaskStillStreams(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)
	task := sendImmediate(t, c, userMessage("watch me"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	first := true
	sawCompleted := false
	for ev, err := range c.SubscribeToTask(context.Background(), &a2a.SubscribeToTaskRequest{ID: task.ID}) {
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		if first {
			if _, ok := ev.(*a2a.Task); !ok {
				t.Fatalf("first event = %T, want *a2a.Task", ev)
			}
			first = false
			if err := a.executor.CompleteTask(string(task.ID), "seen"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if isTerminalEvent(ev) {
			sawCompleted = true
			break
		}
	}
	if !sawCompleted {
		t.Fatal("stream ended without the terminal event")
	}
}

// TestUnsupportedVersionRejected: an explicit A2A-Version other than 1.0
// fails with VersionNotSupported on both bindings before any work happens.
func TestUnsupportedVersionRejected(t *testing.T) {
	a := newTestAgent(t, "peer")
	msg := map[string]any{"message": map[string]any{
		"messageId": "ver-1", "role": "ROLE_USER", "parts": []any{map[string]any{"text": "hi"}},
	}}

	resp := rawCall(t, http.MethodPost, a.url+"/", "99.0", jsonrpcRequest(t, 3, "SendMessage", msg))
	assertRPCError(t, resp, "3", -32009, "VERSION_NOT_SUPPORTED")

	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	rest := rawCall(t, http.MethodPost, a.url+"/message:send", "99.0", body)
	assertRESTError(t, rest, http.StatusBadRequest, "VERSION_NOT_SUPPORTED")

	query := rawCall(t, http.MethodGet, a.url+"/tasks/nope?A2A-Version=0.3", "", nil)
	assertRESTError(t, query, http.StatusBadRequest, "VERSION_NOT_SUPPORTED")

	if n := len(a.store.PeekInbox()); n != 0 {
		t.Fatalf("rejected requests reached the inbox: %d entries", n)
	}
}

// TestMissingOrEmptyVersionRejected: no A2A-Version means 0.3, which a
// 1.0-only agent answers with VersionNotSupported.
func TestMissingOrEmptyVersionRejected(t *testing.T) {
	a := newTestAgent(t, "peer")

	missing := rawCall(t, http.MethodPost, a.url+"/", "", jsonrpcRequest(t, 10, "GetTask", map[string]any{"id": "nope"}))
	assertRPCError(t, missing, "10", -32009, "VERSION_NOT_SUPPORTED")
	missingREST := rawCall(t, http.MethodGet, a.url+"/tasks/nope", "", nil)
	assertRESTError(t, missingREST, http.StatusBadRequest, "VERSION_NOT_SUPPORTED")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.url+"/tasks/nope", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(a2a.SvcParamVersion, "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	assertRESTError(t, rawResponse{status: resp.StatusCode, header: resp.Header, body: body}, http.StatusBadRequest, "VERSION_NOT_SUPPORTED")
}

// TestSupportedVersionPassesThrough: 1.0 and a patch version reach a2a-go,
// from the header or the request parameter.
func TestSupportedVersionPassesThrough(t *testing.T) {
	a := newTestAgent(t, "peer")
	query := rawCall(t, http.MethodGet, a.url+"/tasks/nope?A2A-Version=1.0", "", nil)
	assertRESTError(t, query, http.StatusNotFound, "TASK_NOT_FOUND")
	for _, version := range []string{"1.0", "1.0.3"} {
		resp := rawCall(t, http.MethodPost, a.url+"/", version, jsonrpcRequest(t, 4, "GetTask", map[string]any{"id": "nope"}))
		assertRPCError(t, resp, "4", -32001, "TASK_NOT_FOUND")
		rest := rawCall(t, http.MethodGet, a.url+"/tasks/nope", version, nil)
		assertRESTError(t, rest, http.StatusNotFound, "TASK_NOT_FOUND")
	}
}

func TestCheckProtocolVersion(t *testing.T) {
	cases := []struct {
		header, query string
		wantErr       bool
	}{
		{header: "", wantErr: true},
		{header: "1.0", wantErr: false},
		{header: " 1.0 ", wantErr: false},
		{header: "1.0.7", wantErr: false},
		{header: "v1.0", wantErr: true},
		{header: "1.1", wantErr: true},
		{header: "0.3", wantErr: true},
		{header: "0.5", wantErr: true},
		{header: "2.0", wantErr: true},
		{header: "99.0", wantErr: true},
		{header: "one.zero", wantErr: true},
		{header: "1", wantErr: true},
		{query: "1.0", wantErr: false},
		{query: "2.0", wantErr: true},
		{header: "1.0", query: "2.0", wantErr: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("header=%q,query=%q", tc.header, tc.query), func(t *testing.T) {
			target := "/tasks"
			if tc.query != "" {
				target += "?A2A-Version=" + tc.query
			}
			r := httptest.NewRequest(http.MethodGet, target, http.NoBody)
			if tc.header != "" {
				r.Header.Set(a2a.SvcParamVersion, tc.header)
			}
			err := checkProtocolVersion(r)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkProtocolVersion = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, a2a.ErrVersionNotSupported) {
				t.Fatalf("error %v does not wrap ErrVersionNotSupported", err)
			}
		})
	}
}

// errorShape strips the per-request parts of an error body (message text and
// timestamp) so guard and a2a-go responses can be compared structurally.
func errorShape(t *testing.T, body []byte) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	inner, ok := v["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %s", body)
	}
	inner["message"] = "<message>"
	for _, key := range []string{"data", "details"} {
		items, _ := inner[key].([]any)
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				if meta, ok := m["metadata"].(map[string]any); ok {
					meta["timestamp"] = "<timestamp>"
				}
			}
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestGuardErrorBodiesMatchA2AGo: the guard's TaskNotFound bodies have the
// same shape as the ones a2a-go writes for GetTask on both bindings.
func TestGuardErrorBodiesMatchA2AGo(t *testing.T) {
	a := newTestAgent(t, "peer")

	guardRPC := rawCall(t, http.MethodPost, a.url+"/", "1.0", jsonrpcRequest(t, 5, methodSubscribeToTask, map[string]any{"id": "nope"}))
	sdkRPC := rawCall(t, http.MethodPost, a.url+"/", "1.0", jsonrpcRequest(t, 5, "GetTask", map[string]any{"id": "nope"}))
	if g, s := errorShape(t, guardRPC.body), errorShape(t, sdkRPC.body); g != s {
		t.Errorf("JSON-RPC error shape differs:\nguard: %s\na2a-go: %s", g, s)
	}

	guardREST := rawCall(t, http.MethodPost, a.url+"/tasks/nope:subscribe", "1.0", nil)
	sdkREST := rawCall(t, http.MethodGet, a.url+"/tasks/nope", "1.0", nil)
	if g, s := errorShape(t, guardREST.body), errorShape(t, sdkREST.body); g != s {
		t.Errorf("REST error shape differs:\nguard: %s\na2a-go: %s", g, s)
	}
	if guardREST.status != sdkREST.status {
		t.Errorf("REST status guard=%d a2a-go=%d", guardREST.status, sdkREST.status)
	}
}

func TestSubscribeTaskID(t *testing.T) {
	cases := map[string]string{
		"/tasks/abc:subscribe": "abc",
		"/tasks/abc":           "",
		"/tasks/:subscribe":    "",
		"/tasks/abc:cancel":    "",
		"/tasks/abc/pushNotificationConfigs:subscribe": "",
		"/message:send": "",
	}
	for path, want := range cases {
		id, ok := subscribeTaskID(path)
		if string(id) != want || ok != (want != "") {
			t.Errorf("subscribeTaskID(%q) = %q, %v; want %q", path, id, ok, want)
		}
	}
}
