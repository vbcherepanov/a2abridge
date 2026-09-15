package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv/push"
)

const waitTimeout = 5 * time.Second

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testAgent is a bridge-shaped A2A server on httptest: the real Store,
// Executor, TTLStore, PushSender and NewA2AHTTPHandler.
type testAgent struct {
	store       *Store
	executor    *Executor
	tasks       *TTLStore
	pushConfigs *push.InMemoryPushConfigStore
	card        *a2a.AgentCard
	url         string
}

func newTestAgent(t *testing.T, name string) *testAgent {
	t.Helper()
	return newTestAgentWithServer(t, name, nil)
}

// newTestAgentWithServer lets a test adjust the http.Server before it starts.
func newTestAgentWithServer(t *testing.T, name string, configure func(*http.Server)) *testAgent {
	t.Helper()
	log := discardLog()
	store := NewStore()
	t.Cleanup(store.Close)
	pushConfigs := push.NewInMemoryStore()
	tasks := NewTTLStore(pushConfigs, log)
	t.Cleanup(tasks.Close)
	executor := NewExecutor(store, log)

	srv := httptest.NewUnstartedServer(nil)
	url := "http://" + srv.Listener.Addr().String()
	card := &a2a.AgentCard{
		Name:        name,
		Description: "test agent " + name,
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(url, a2a.TransportProtocolJSONRPC),
			a2a.NewAgentInterface(url, a2a.TransportProtocolHTTPJSON),
		},
		Capabilities:       a2a.AgentCapabilities{Streaming: true, PushNotifications: true},
		DefaultInputModes:  []string{TextMediaType},
		DefaultOutputModes: []string{TextMediaType},
		Skills:             []a2a.AgentSkill{},
		Version:            "test",
	}
	sender := NewPushSender(nil, true, log)
	t.Cleanup(func() { closePushSender(t, sender) })
	handler, err := NewA2AHTTPHandler(card, executor, tasks, pushConfigs, sender, log)
	if err != nil {
		t.Fatalf("a2a handler: %v", err)
	}
	srv.Config.Handler = handler
	if configure != nil {
		configure(srv.Config)
	}
	srv.Start()
	t.Cleanup(srv.Close)

	return &testAgent{store: store, executor: executor, tasks: tasks, pushConfigs: pushConfigs, card: card, url: url}
}

// client returns an a2a-go client connected to the agent through its card.
func (a *testAgent) client(t *testing.T) *a2aclient.Client {
	t.Helper()
	peers := NewPeers(nil, discardLog())
	c, err := peers.Client(context.Background(), a.url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { peers.Release(c) })
	return c
}

func userMessage(text string) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.Metadata = map[string]any{"from": "peer-a"}
	return msg
}

func sendImmediate(t *testing.T, c *a2aclient.Client, msg *a2a.Message) *a2a.Task {
	t.Helper()
	res, err := c.SendMessage(context.Background(), &a2a.SendMessageRequest{
		Message: msg,
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("send result = %T, want *a2a.Task", res)
	}
	return task
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitTaskState(t *testing.T, c *a2aclient.Client, id a2a.TaskID, state a2a.TaskState) *a2a.Task {
	t.Helper()
	var last *a2a.Task
	waitFor(t, "task "+string(id)+" in state "+string(state), func() bool {
		task, err := c.GetTask(context.Background(), &a2a.GetTaskRequest{ID: id})
		if err != nil {
			return false
		}
		last = task
		return task.Status.State == state
	})
	return last
}

// inboxEntryFor waits until the inbox holds an entry for the task.
func (a *testAgent) inboxEntryFor(t *testing.T, id a2a.TaskID) InboxEntry {
	t.Helper()
	var found InboxEntry
	waitFor(t, "inbox entry for "+string(id), func() bool {
		for _, e := range a.store.PeekInbox() {
			if e.TaskID == string(id) {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

// firstInboxEntry waits until any inbound entry is queued.
func (a *testAgent) firstInboxEntry(t *testing.T) InboxEntry {
	t.Helper()
	var found InboxEntry
	waitFor(t, "an inbox entry", func() bool {
		entries := a.store.PeekInbox()
		if len(entries) == 0 {
			return false
		}
		found = entries[0]
		return true
	})
	return found
}
