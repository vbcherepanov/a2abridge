package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// TestReturnImmediatelySendThenComplete: a ReturnImmediately send leaves the
// task WORKING with an inbox entry; CompleteTask finishes it with the reply
// as artifact and status message, clears the inbox and releases the waiter.
func TestReturnImmediatelySendThenComplete(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	task := sendImmediate(t, c, userMessage("ping"))
	if task.Status.State.Terminal() {
		t.Fatalf("returned state = %s, want non-terminal", task.Status.State)
	}
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	entry := a.inboxEntryFor(t, task.ID)
	if entry.Text != "ping" || entry.From != "peer-a" || entry.ContextID != task.ContextID {
		t.Fatalf("inbox entry = %+v", entry)
	}

	if err := a.executor.CompleteTask(string(task.ID), "pong"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	done := waitTaskState(t, c, task.ID, a2a.TaskStateCompleted)
	if got := extractReplyText(done); got != "pong" {
		t.Errorf("artifact text = %q, want pong", got)
	}
	if len(done.Artifacts) != 1 || done.Artifacts[0].Name != replyArtifactName {
		t.Errorf("artifacts = %+v, want one %q artifact", done.Artifacts, replyArtifactName)
	}
	if done.Status.Message == nil || done.Status.Message.Role != a2a.MessageRoleAgent {
		t.Errorf("status message = %+v, want agent reply", done.Status.Message)
	}
	if n := len(a.store.PeekInbox()); n != 0 {
		t.Errorf("inbox size after complete = %d, want 0", n)
	}
	waitFor(t, "waiter release", func() bool { return a.executor.pendingWaiters() == 0 })
}

// TestBlockingSendCompletesOnConcurrentReply: a blocking send returns the
// COMPLETED task once the host answers while the call is in flight.
func TestBlockingSendCompletesOnConcurrentReply(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	errc := make(chan error, 1)
	go func() {
		entry := a.firstInboxEntry(t)
		errc <- a.executor.CompleteTask(entry.TaskID, "done")
	}()

	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: userMessage("question")})
	if err != nil {
		t.Fatalf("blocking send: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("result = %T, want *a2a.Task", res)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("state = %s, want COMPLETED", task.Status.State)
	}
	if got := extractReplyText(task); got != "done" {
		t.Errorf("reply = %q, want done", got)
	}
	waitFor(t, "waiter release", func() bool { return a.executor.pendingWaiters() == 0 })
}

// TestCancelReleasesWaiter: CancelTask moves the task to CANCELED, the
// blocked execution exits, and a late CompleteTask reports ErrTaskNotFound.
func TestCancelReleasesWaiter(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	task := sendImmediate(t, c, userMessage("long job"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	canceled, err := c.CancelTask(context.Background(), &a2a.CancelTaskRequest{ID: task.ID})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("state = %s, want CANCELED", canceled.Status.State)
	}
	waitFor(t, "waiter release after cancel", func() bool { return a.executor.pendingWaiters() == 0 })

	if err := a.executor.CompleteTask(string(task.ID), "too late"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CompleteTask after cancel = %v, want ErrTaskNotFound", err)
	}
}

// TestStreamingSendSeesLifecycle: a streaming send observes the task,
// WORKING, the reply artifact and COMPLETED, in that order.
func TestStreamingSendSeesLifecycle(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	errc := make(chan error, 1)
	go func() {
		entry := a.firstInboxEntry(t)
		errc <- a.executor.CompleteTask(entry.TaskID, "streamed reply")
	}()

	var working, artifact, completed = -1, -1, -1
	i := 0
	for ev, err := range c.SendStreamingMessage(context.Background(), &a2a.SendMessageRequest{Message: userMessage("stream me")}) {
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		switch v := ev.(type) {
		case *a2a.TaskStatusUpdateEvent:
			switch v.Status.State {
			case a2a.TaskStateWorking:
				working = i
			case a2a.TaskStateCompleted:
				completed = i
			default:
			}
		case *a2a.TaskArtifactUpdateEvent:
			if len(v.Artifact.Parts) == 1 && v.Artifact.Parts[0].Text() == "streamed reply" {
				artifact = i
			}
		default:
		}
		i++
	}
	if err := <-errc; err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	if working < 0 || artifact <= working || completed <= artifact {
		t.Fatalf("event order working=%d artifact=%d completed=%d, want increasing", working, artifact, completed)
	}
}

// TestFollowUpToWorkingTaskRejected: A2A 1.0 via a2a-go runs one execution
// per task, so a message to a task that is still WORKING is refused.
func TestFollowUpToWorkingTaskRejected(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	task := sendImmediate(t, c, userMessage("first"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	followUp := userMessage("second")
	followUp.TaskID = task.ID
	if _, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: followUp,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	}); err == nil {
		t.Fatal("follow-up to a working task succeeded, want an error")
	}

	if err := a.executor.CompleteTask(string(task.ID), "answer"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	waitTaskState(t, c, task.ID, a2a.TaskStateCompleted)
}

// TestDuplicateMessageIDRejected: a redelivered messageId is queued once;
// the redelivery's task is REJECTED instead of waiting for a reply nobody sees.
func TestDuplicateMessageIDRejected(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	first := userMessage("once")
	task := sendImmediate(t, c, first)
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	again := userMessage("once")
	again.ID = first.ID
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{Message: again})
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	dup, ok := res.(*a2a.Task)
	if !ok || dup.Status.State != a2a.TaskStateRejected {
		t.Fatalf("redelivery result = %+v, want REJECTED task", res)
	}
	if n := len(a.store.PeekInbox()); n != 1 {
		t.Fatalf("inbox size = %d, want 1", n)
	}
	if err := a.executor.CompleteTask(string(task.ID), "answer"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
	waitFor(t, "waiter release", func() bool { return a.executor.pendingWaiters() == 0 })
}

// TestPushConfigReceivesCompletion: a webhook registered by the client gets
// the completion event with the notification token.
func TestPushConfigReceivesCompletion(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)

	type delivery struct {
		token string
		event a2a.Event
	}
	deliveries := make(chan delivery, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var sr a2a.StreamResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		deliveries <- delivery{token: r.Header.Get(pushTokenHeader), event: sr.Event}
	}))
	defer hook.Close()

	task := sendImmediate(t, c, userMessage("notify me"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)
	if _, err := c.CreateTaskPushConfig(context.Background(), &a2a.PushConfig{
		TaskID: task.ID, URL: hook.URL, Token: "tok-1",
	}); err != nil {
		t.Fatalf("create push config: %v", err)
	}
	if err := a.executor.CompleteTask(string(task.ID), "pushed"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}

	waitFor(t, "completion webhook", func() bool {
		select {
		case d := <-deliveries:
			if d.token != "tok-1" {
				t.Errorf("token header = %q, want tok-1", d.token)
			}
			ev, ok := d.event.(*a2a.TaskStatusUpdateEvent)
			return ok && ev.TaskID == task.ID && ev.Status.State == a2a.TaskStateCompleted
		default:
			return false
		}
	})
}

// TestRESTBindingGetTask: the HTTP+JSON binding serves GET /tasks/{id}.
func TestRESTBindingGetTask(t *testing.T) {
	a := newTestAgent(t, "peer")
	c := a.client(t)
	task := sendImmediate(t, c, userMessage("rest"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.url+"/tasks/"+string(task.ID), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("A2A-Version", string(a2a.Version))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET task: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %s, body = %s", resp.Status, body)
	}
	var got a2a.Task
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != task.ID || got.Status.State != a2a.TaskStateWorking {
		t.Fatalf("REST task = %+v", got)
	}

	if err := a.executor.CompleteTask(string(task.ID), "bye"); err != nil {
		t.Fatalf("CompleteTask: %v", err)
	}
}

// TestAgentCardServesBothInterfaces: the well-known card advertises the
// JSON-RPC and HTTP+JSON bindings at A2A 1.0.
func TestAgentCardServesBothInterfaces(t *testing.T) {
	a := newTestAgent(t, "peer")
	card, err := NewPeers(nil, discardLog()).Resolve(context.Background(), a.url)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	protocols := map[a2a.TransportProtocol]bool{}
	for _, iface := range card.SupportedInterfaces {
		if iface.URL != a.url || iface.ProtocolVersion != a2a.Version {
			t.Errorf("interface = %+v, want url %s version %s", iface, a.url, a2a.Version)
		}
		protocols[iface.ProtocolBinding] = true
	}
	if !protocols[a2a.TransportProtocolJSONRPC] || !protocols[a2a.TransportProtocolHTTPJSON] {
		t.Fatalf("protocols = %v, want JSONRPC and HTTP+JSON", protocols)
	}
}

// TestOperationalEndpoints: /healthz and /metrics stay on the agent mux.
func TestOperationalEndpoints(t *testing.T) {
	a := newTestAgent(t, "peer")
	for path, want := range map[string]string{"/healthz": "ok", "/metrics": "a2abridge_inbox_size"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, a.url+path, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %s %q, want 200 containing %q", path, resp.Status, body, want)
		}
	}
}

// TestCompleteTaskReplyPending: a second reply before the execution consumed
// the first is refused instead of silently overwriting it.
func TestCompleteTaskReplyPending(t *testing.T) {
	store := NewStore()
	defer store.Close()
	e := NewExecutor(store, discardLog())
	if _, err := e.registerWaiter("t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.registerWaiter("t1"); !errors.Is(err, errWaiterExists) {
		t.Fatalf("second registerWaiter = %v, want errWaiterExists", err)
	}
	if err := e.CompleteTask("t1", "one"); err != nil {
		t.Fatalf("first CompleteTask: %v", err)
	}
	if err := e.CompleteTask("t1", "two"); !errors.Is(err, ErrReplyPending) {
		t.Fatalf("second CompleteTask = %v, want ErrReplyPending", err)
	}
	e.unregisterWaiter("t1")
	if n := e.pendingWaiters(); n != 0 {
		t.Fatalf("pendingWaiters = %d, want 0", n)
	}
}
