package agent

// SSE fast-path for outgoing-reply delivery. See subscribeOutgoingReply
// at the bottom of this file.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/vbcherepanov/a2abridge/internal/a2a"
	"github.com/vbcherepanov/a2abridge/internal/security"
)

// MCPDeps ties the local store, own card and directory URL so MCP tools can act.
type MCPDeps struct {
	Store        *Store
	OwnCard      a2a.AgentCard
	DirectoryURL string // e.g. http://127.0.0.1:7777
}

// RegisterTools attaches a2a_* MCP tools that use the A2A protocol as transport.
func RegisterTools(s *server.MCPServer, d *MCPDeps) {
	s.AddTool(
		mcp.NewTool("a2a_whoami",
			mcp.WithDescription("Return this agent's own A2A Agent Card."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			b, _ := json.MarshalIndent(d.OwnCard, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_list_agents",
			mcp.WithDescription("Discover peer A2A agents via the directory. Returns each peer's Agent Card."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peers, err := listPeers(ctx, d.DirectoryURL, d.OwnCard.URL)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, _ := json.MarshalIndent(peers, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_send_message",
			mcp.WithDescription("Call a2a.SendMessage on a peer agent (fire-and-forget or blocking per `blocking`). Returns the resulting Task or Message."),
			mcp.WithString("peer_url", mcp.Required(), mcp.Description("Base URL of the peer A2A agent (e.g. http://127.0.0.1:49152)")),
			mcp.WithString("text", mcp.Required(), mcp.Description("Text content of the message")),
			mcp.WithBoolean("blocking", mcp.Description("Ask the peer to block until terminal state (default false)")),
			mcp.WithString("context_id", mcp.Description("Existing contextId to continue a conversation")),
			mcp.WithString("task_id", mcp.Description("Existing taskId to continue a task")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, _ := req.RequireString("peer_url")
			text, _ := req.RequireString("text")
			blocking, _ := req.RequireBool("blocking")
			ctxID, _ := req.RequireString("context_id")
			taskID, _ := req.RequireString("task_id")

			// Screen for AWS keys, GitHub tokens, JWTs, PEM private keys
			// etc. The screener replaces matches with [REDACTED:<name>] so
			// the message still goes through with usable context — only
			// the secret is stripped. The MCP tool result mentions any
			// redaction so the model can warn the user.
			redacted, hits := security.Screen(text)
			text = redacted

			client := a2a.NewClient(peerURL)
			meta := map[string]any{"from": d.OwnCard.Name, "fromUrl": d.OwnCard.URL}
			if len(hits) > 0 {
				meta["redactions"] = security.FormatMatches(hits)
			}
			msg := a2a.Message{
				MessageID: uuid.NewString(),
				ContextID: ctxID,
				TaskID:    taskID,
				Role:      a2a.RoleUser,
				Parts:     []a2a.Part{{Text: text}},
				Metadata:  meta,
			}
			params := a2a.MessageSendParams{
				Message: msg,
				Configuration: &a2a.MessageSendConfiguration{
					Blocking:            blocking,
					AcceptedOutputModes: []string{"text/plain"},
				},
			}
			res, err := client.SendMessage(ctx, params)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// Регистрируем исходящую задачу для фонового опроса — чтобы
			// когда пир ответит, ответ автоматически попал в inbox и hook
			// подсунул его пользователю в следующий turn.
			if res != nil && res.Task != nil {
				peerName := ""
				if c, err := a2a.NewClient(peerURL).FetchAgentCard(ctx); err == nil {
					peerName = c.Name
				}
				d.Store.TrackOutgoing(res.Task.ID, peerURL, peerName, text)
				// Open an SSE subscription on the peer so the reply lands
				// the moment the peer reaches a terminal state — no need to
				// wait for the 5-second polling tick. The poll loop is still
				// running as a safety net in case this stream drops.
				go subscribeOutgoingReply(peerURL, res.Task.ID, d.Store)
			}
			b, _ := json.MarshalIndent(res, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_send_streaming",
			mcp.WithDescription("Call a2a.SendStreamingMessage and wait until the task reaches a terminal state. Returns aggregated artifacts + final status."),
			mcp.WithString("peer_url", mcp.Required()),
			mcp.WithString("text", mcp.Required()),
			mcp.WithNumber("timeout_s", mcp.Description("Max seconds to wait (default 300)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, _ := req.RequireString("peer_url")
			text, _ := req.RequireString("text")
			timeout := 300 * time.Second
			if v, err := req.RequireFloat("timeout_s"); err == nil && v > 0 {
				timeout = time.Duration(v * float64(time.Second))
			}

			client := a2a.NewClient(peerURL)
			params := a2a.MessageSendParams{
				Message: a2a.Message{
					MessageID: uuid.NewString(),
					Role:      a2a.RoleUser,
					Parts:     []a2a.Part{{Text: text}},
					Metadata:  map[string]any{"from": d.OwnCard.Name, "fromUrl": d.OwnCard.URL},
				},
			}

			sctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			ch := make(chan a2a.StreamResponse, 16)
			errCh := make(chan error, 1)
			go func() { errCh <- client.SendStreamingMessage(sctx, params, ch); close(ch) }()

			var collected []a2a.StreamResponse
			for ev := range ch {
				collected = append(collected, ev)
			}
			if err := <-errCh; err != nil && err != context.Canceled {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, _ := json.MarshalIndent(collected, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_get_task",
			mcp.WithDescription("Call a2a.GetTask on a peer."),
			mcp.WithString("peer_url", mcp.Required()),
			mcp.WithString("task_id", mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, _ := req.RequireString("peer_url")
			taskID, _ := req.RequireString("task_id")
			t, err := a2a.NewClient(peerURL).GetTask(ctx, taskID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, _ := json.MarshalIndent(t, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_cancel_task",
			mcp.WithDescription("Call a2a.CancelTask on a peer."),
			mcp.WithString("peer_url", mcp.Required()),
			mcp.WithString("task_id", mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, _ := req.RequireString("peer_url")
			taskID, _ := req.RequireString("task_id")
			t, err := a2a.NewClient(peerURL).CancelTask(ctx, taskID)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			b, _ := json.MarshalIndent(t, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_inbox",
			mcp.WithDescription("Drain or peek pending messages that peers have sent to this agent. Each entry includes taskId so you can a2a_complete_task after answering."),
			mcp.WithBoolean("peek", mcp.Description("If true, read without clearing")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peek, _ := req.RequireBool("peek")
			var msgs []a2a.Message
			if peek {
				msgs = d.Store.PeekInbox()
			} else {
				msgs = d.Store.DrainInbox()
			}
			if len(msgs) == 0 {
				return mcp.NewToolResultText("[]"), nil
			}
			b, _ := json.MarshalIndent(msgs, "", "  ")
			return mcp.NewToolResultText(string(b)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_complete_task",
			mcp.WithDescription("Attach a reply as an Artifact and mark the local task COMPLETED. Use this to answer a peer's incoming message after processing it."),
			mcp.WithString("task_id", mcp.Required()),
			mcp.WithString("text", mcp.Required(), mcp.Description("Reply body")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			taskID, _ := req.RequireString("task_id")
			text, _ := req.RequireString("text")
			if err := d.Store.CompleteTask(taskID, text); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return mcp.NewToolResultText("completed"), nil
		},
	)
}

// PeerInfo combines directory entry + fetched Agent Card.
type PeerInfo struct {
	URL  string         `json:"url"`
	Card *a2a.AgentCard `json:"card,omitempty"`
	Err  string         `json:"error,omitempty"`
}

func listPeers(ctx context.Context, directoryURL, selfURL string) ([]PeerInfo, error) {
	if directoryURL == "" {
		return nil, fmt.Errorf("A2A_DIRECTORY not set")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(directoryURL, "/")+"/agents", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var entries []struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}
	out := make([]PeerInfo, 0, len(entries))
	for _, e := range entries {
		if e.URL == selfURL {
			continue
		}
		info := PeerInfo{URL: e.URL}
		card, err := a2a.NewClient(e.URL).FetchAgentCard(ctx)
		if err != nil {
			info.Err = err.Error()
		} else {
			info.Card = card
		}
		out = append(out, info)
	}
	return out, nil
}

// Heartbeat periodically re-registers this agent with the directory.
func Heartbeat(ctx context.Context, directoryURL, selfURL string) {
	body, _ := json.Marshal(map[string]string{"url": selfURL})
	do := func(path string) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(directoryURL, "/")+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	do("/register")
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			do("/unregister")
			return
		case <-t.C:
			do("/heartbeat")
		}
	}
}

// subscribeOutgoingReply opens an a2a.SubscribeToTask SSE stream on the
// peer for the just-created outbound task. As soon as the peer's task
// reaches a terminal state we resolve the full Task via GetTask and hand
// it to Store.IngestOutgoingTerminal — that drops a synthetic reply into
// our inbox without waiting for the 5-second polling fallback.
//
// Network jitter is fine: if the SSE connection drops mid-stream the
// polling loop in cmd/a2abridge/bridge.go will catch the same task on
// its next tick. We spend at most an HTTP keep-alive's worth of memory
// per outstanding outbound task.
func subscribeOutgoingReply(peerURL, taskID string, store *Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := a2a.NewClient(peerURL)
	ch := make(chan a2a.StreamResponse, 4)

	go func() {
		defer close(ch)
		_ = client.SubscribeToTask(ctx, taskID, ch)
	}()

	terminal := false
	for ev := range ch {
		// Two paths reach a terminal: (a) statusUpdate with final=true and a
		// terminal state, (b) a final 'task' message after the peer aggregated.
		if ev.StatusUpdate != nil && ev.StatusUpdate.Final {
			if isTerminalState(ev.StatusUpdate.Status.State) {
				terminal = true
				break
			}
		}
		if ev.Task != nil && isTerminalState(ev.Task.Status.State) {
			terminal = true
			break
		}
	}
	if !terminal {
		return
	}
	// Resolve the full task — statusUpdate carries a state but not artifacts;
	// we want the artefact text in the synthesized reply.
	resolveCtx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	t, err := client.GetTask(resolveCtx, taskID)
	if err != nil || t == nil {
		return
	}
	store.IngestOutgoingTerminal(t)
}

func isTerminalState(s a2a.TaskState) bool {
	switch s {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed,
		a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	}
	return false
}
