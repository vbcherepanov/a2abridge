package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/mark3labs/mcp-go/server"
)

const testSecret = "ghp_0123456789012345678901234567890123456789"

// callTool drives a registered MCP tool through the real JSON-RPC
// dispatcher so the test exercises the exact code path the IDE uses.
func callTool(t *testing.T, s *server.MCPServer, name string, args map[string]any) string {
	t.Helper()
	params, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":%s}`, params))
	resp := s.HandleMessage(context.Background(), raw)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestMCP(t *testing.T, store *Store, executor *Executor) *server.MCPServer {
	t.Helper()
	mcpSrv := server.NewMCPServer("test", "0.0.0")
	RegisterTools(mcpSrv, &MCPDeps{
		Lifetime:     t.Context(),
		Store:        store,
		Executor:     executor,
		Peers:        NewPeers(nil, discardLog()),
		OwnCard:      &a2a.AgentCard{Name: "self"},
		SelfURL:      "http://self.invalid",
		DirectoryURL: "http://127.0.0.1:1", // unused by the tools under test
		Log:          discardLog(),
	})
	return mcpSrv
}

// TestCompleteTaskToolScreensSecrets — a2a_complete_task is an outbound
// path (the reply leaves via streams / webhooks), so it must run through the
// same secret screen as a2a_send_message.
func TestCompleteTaskToolScreensSecrets(t *testing.T) {
	a := newTestAgent(t, "self")
	c := a.client(t)
	task := sendImmediate(t, c, userMessage("what's the token?"))
	waitTaskState(t, c, task.ID, a2a.TaskStateWorking)

	resp := callTool(t, newTestMCP(t, a.store, a.executor), "a2a_complete_task", map[string]any{
		"task_id": string(task.ID),
		"text":    "use " + testSecret + " for auth",
	})
	if strings.Contains(resp, testSecret) {
		t.Error("tool result leaked the raw secret")
	}
	if !strings.Contains(resp, "completed") {
		t.Fatalf("tool result = %s, want completed", resp)
	}

	done := waitTaskState(t, c, task.ID, a2a.TaskStateCompleted)
	reply := extractReplyText(done)
	if strings.Contains(reply, testSecret) {
		t.Error("raw secret reached the task artifact")
	}
	if !strings.Contains(reply, "[REDACTED:github-token]") {
		t.Errorf("reply not redacted: %q", reply)
	}
}

// TestSendStreamingToolScreensSecrets — a2a_send_streaming must not be a
// side door for unredacted text: the peer's inbox must only ever see the
// redacted form.
func TestSendStreamingToolScreensSecrets(t *testing.T) {
	peer := newTestAgent(t, "peer")
	inbound := make(chan InboxEntry, 1)
	// Auto-complete incoming tasks so the streaming call terminates fast.
	peer.store.OnIncoming = func(e *InboxEntry) {
		inbound <- *e
		if err := peer.executor.CompleteTask(e.TaskID, "ack"); err != nil {
			t.Errorf("peer CompleteTask: %v", err)
		}
	}

	local := NewStore()
	defer local.Close()
	resp := callTool(t, newTestMCP(t, local, NewExecutor(local, discardLog())), "a2a_send_streaming", map[string]any{
		"peer_url":  peer.url,
		"text":      "deploy key: " + testSecret,
		"timeout_s": 10,
	})
	if strings.Contains(resp, testSecret) {
		t.Error("streaming tool result leaked the raw secret")
	}
	if !strings.Contains(resp, string(a2a.TaskStateCompleted)) {
		t.Errorf("streaming tool result has no completed event: %s", resp)
	}

	var got InboxEntry
	select {
	case got = <-inbound:
	default:
		t.Fatal("peer never received the streamed message")
	}
	if strings.Contains(got.Text, testSecret) {
		t.Error("raw secret crossed the wire via send_streaming")
	}
	if !strings.Contains(got.Text, "[REDACTED:github-token]") {
		t.Errorf("peer received unredacted text: %q", got.Text)
	}
	if got.From != "self" {
		t.Errorf("peer saw from = %q, want self", got.From)
	}
}

// TestSendMessageToolDeliversReplyToInbox — a non-blocking a2a_send_message
// tracks the outgoing task; when the peer answers, the reply subscription
// drops a synthetic entry into the sender's inbox.
func TestSendMessageToolDeliversReplyToInbox(t *testing.T) {
	peer := newTestAgent(t, "peer")
	local := NewStore()
	defer local.Close()

	resp := callTool(t, newTestMCP(t, local, NewExecutor(local, discardLog())), "a2a_send_message", map[string]any{
		"peer_url": peer.url,
		"text":     "ping " + testSecret,
	})
	if strings.Contains(resp, testSecret) || strings.Contains(resp, `"isError":true`) {
		t.Fatalf("send_message result = %s", resp)
	}

	entry := peer.firstInboxEntry(t)
	if !strings.Contains(entry.Text, "[REDACTED:github-token]") || entry.From != "self" {
		t.Fatalf("peer inbox entry = %+v", entry)
	}
	if err := peer.executor.CompleteTask(entry.TaskID, "pong"); err != nil {
		t.Fatalf("peer CompleteTask: %v", err)
	}

	var reply InboxEntry
	waitFor(t, "synthetic reply in the sender's inbox", func() bool {
		local.mu.Lock()
		defer local.mu.Unlock()
		if len(local.inbox) == 0 {
			return false
		}
		reply = local.inbox[0]
		return true
	})
	if reply.TaskID != entry.TaskID || reply.Kind != KindOutgoingReply || reply.From != "peer" {
		t.Fatalf("synthetic reply = %+v", reply)
	}
	if !strings.HasPrefix(reply.Text, "[ОТВЕТ от peer на твой вопрос «ping [REDACTED:github-token]»]") || !strings.HasSuffix(reply.Text, "\npong") {
		t.Errorf("synthetic reply text = %q", reply.Text)
	}
}
