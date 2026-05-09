package a2a

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestRESTRoundTripCovers each non-streaming REST verb with a single
// table-driven test. Streaming routes are exercised in TestRESTStreaming
// below.
func TestRESTRoundTrip(t *testing.T) {
	h := &fakeHandler{
		sendTask:   &Task{ID: "rest-1", Status: TaskStatus{State: TaskStateSubmitted}},
		getTask:    &Task{ID: "rest-1", Status: TaskStatus{State: TaskStateCompleted}},
		cancelTask: &Task{ID: "rest-1", Status: TaskStatus{State: TaskStateCanceled}},
		listTasks:  []Task{{ID: "rest-1"}, {ID: "rest-2"}},
	}
	ts := newTestServer(t, h)
	defer ts.Close()

	// SendMessage
	body, _ := json.Marshal(MessageSendParams{
		Message: Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: "hi"}}},
	})
	resp, err := http.Post(ts.URL+"/v1/tasks", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("POST /v1/tasks status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GetTask
	resp, err = http.Get(ts.URL + "/v1/tasks/rest-1")
	if err != nil {
		t.Fatal(err)
	}
	var got Task
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Status.State != TaskStateCompleted {
		t.Errorf("GET /v1/tasks/rest-1 state = %v, want COMPLETED", got.Status.State)
	}

	// ListTasks
	resp, err = http.Get(ts.URL + "/v1/tasks")
	if err != nil {
		t.Fatal(err)
	}
	var list []Task
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 2 {
		t.Errorf("GET /v1/tasks returned %d, want 2", len(list))
	}

	// CancelTask
	resp, err = http.Post(ts.URL+"/v1/tasks/rest-1/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Status.State != TaskStateCanceled {
		t.Errorf("POST cancel state = %v, want CANCELED", got.Status.State)
	}

	// AgentCard
	resp, err = http.Get(ts.URL + "/v1/agent")
	if err != nil {
		t.Fatal(err)
	}
	var card AgentCard
	_ = json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	if card.Name != "test-agent" {
		t.Errorf("GET /v1/agent name = %q", card.Name)
	}
}

func TestRESTGetTaskNotFoundIs404(t *testing.T) {
	ts := newTestServer(t, &fakeHandler{getErr: ErrTaskNotFound})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/tasks/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestRESTPushNotSupportedReturns501 verifies the spec-compliant fallback
// when the handler does not implement PushHandler.
func TestRESTPushNotSupportedReturns501(t *testing.T) {
	ts := newTestServer(t, &fakeHandler{}) // doesn't implement PushHandler
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/tasks/x/push", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestRESTSendMessageBadJSONIs400(t *testing.T) {
	ts := newTestServer(t, &fakeHandler{})
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/tasks", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
