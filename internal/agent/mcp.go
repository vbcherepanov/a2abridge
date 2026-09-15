package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/vbcherepanov/a2abridge/v4/internal/metrics"
	"github.com/vbcherepanov/a2abridge/v4/internal/security"
)

const (
	// directoryRequestTimeout bounds every directory HTTP call (register,
	// heartbeat, /agents listing) and each peer agent-card fetch.
	directoryRequestTimeout = 5 * time.Second

	// peerRequestTimeout bounds unary calls to a peer (send, get, cancel),
	// including a blocking send waiting for the peer's reply.
	peerRequestTimeout = 60 * time.Second

	// defaultStreamingTimeout is a2a_send_streaming's default timeout_s.
	defaultStreamingTimeout = 300 * time.Second

	// outgoingSubscribeTimeout bounds the reply subscription per outbound task.
	outgoingSubscribeTimeout = 10 * time.Minute

	// outgoingResolveTimeout bounds the GetTask after the subscription ends.
	outgoingResolveTimeout = 5 * time.Second

	// peerCardFanOut bounds concurrent agent-card fetches in a2a_list_agents.
	peerCardFanOut = 8
)

// MCPDeps ties the local agent, peer clients and directory so MCP tools can act.
type MCPDeps struct {
	// Lifetime is the bridge's lifetime context: background work started by a
	// tool (reply subscriptions) stops when it is canceled. nil = never canceled.
	Lifetime     context.Context
	Store        *Store
	Executor     *Executor
	Peers        *Peers
	OwnCard      *a2a.AgentCard
	SelfURL      string
	DirectoryURL string       // e.g. http://127.0.0.1:7777
	Log          *slog.Logger // optional; nil falls back to slog.Default()
}

func (d *MCPDeps) lifetime() context.Context {
	if d.Lifetime != nil {
		return d.Lifetime
	}
	return context.Background()
}

func (d *MCPDeps) logger() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// screenOutbound applies the PII / secret screen to text that is about to
// leave this agent. Every outbound path (send_message, send_streaming,
// complete_task) MUST go through this single choke point so a new tool
// can't silently bypass the screen.
func screenOutbound(text string, log *slog.Logger) (string, []security.Match) {
	redacted, hits := security.Screen(text)
	if len(hits) > 0 {
		log.Warn("outbound text redacted", "summary", security.FormatMatches(hits), "count", len(hits))
	}
	return redacted, hits
}

// outboundMessage builds the user message sent to a peer from screened text.
func (d *MCPDeps) outboundMessage(text string, hits []security.Match) *a2a.Message {
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	msg.Metadata = map[string]any{"from": d.OwnCard.Name, "fromUrl": d.SelfURL}
	if len(hits) > 0 {
		msg.Metadata["redactions"] = security.FormatMatches(hits)
	}
	return msg
}

// jsonResult renders v as an indented JSON tool result.
func jsonResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultError("encode result: " + err.Error()), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// RegisterTools attaches a2a_* MCP tools that use the A2A protocol as transport.
func RegisterTools(s *server.MCPServer, d *MCPDeps) {
	s.AddTool(
		mcp.NewTool("a2a_whoami",
			mcp.WithDescription("Return this agent's own A2A Agent Card."),
		),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return jsonResult(d.OwnCard)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_list_agents",
			mcp.WithDescription("Discover peer A2A agents via the directory. Returns each peer's Agent Card."),
		),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peers, err := listPeers(ctx, d.Peers, d.DirectoryURL, d.SelfURL)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(peers)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_send_message",
			mcp.WithDescription("Call SendMessage on a peer agent (returns immediately with the WORKING task, or blocks until the reply when `blocking` is true). Returns the resulting Task or Message."),
			mcp.WithString("peer_url", mcp.Required(), mcp.Description("Base URL of the peer A2A agent (e.g. http://127.0.0.1:49152)")),
			mcp.WithString("text", mcp.Required(), mcp.Description("Text content of the message")),
			mcp.WithBoolean("blocking", mcp.Description("Wait until the peer's task reaches a terminal state (default false)")),
			mcp.WithString("context_id", mcp.Description("Existing contextId to continue a conversation; each message still starts a new task")),
			mcp.WithString("task_id", mcp.Description("Existing taskId, only for a task awaiting input — A2A 1.0 rejects messages to a task that is still being worked on or already finished")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, err := req.RequireString("peer_url")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}

			// Screen for AWS keys, GitHub tokens, JWTs, PEM private keys
			// etc. The screener replaces matches with [REDACTED:<name>] so
			// the message still goes through with usable context — only
			// the secret is stripped. The message metadata mentions any
			// redaction so the peer can warn its user.
			redacted, hits := screenOutbound(text, d.logger())
			msg := d.outboundMessage(redacted, hits)
			msg.ContextID = req.GetString("context_id", "")
			msg.TaskID = a2a.TaskID(req.GetString("task_id", ""))

			rctx, cancel := context.WithTimeout(ctx, peerRequestTimeout)
			defer cancel()
			client, err := d.Peers.Client(rctx, peerURL)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			defer d.Peers.Release(client)

			res, err := client.SendMessage(rctx, &a2a.SendMessageRequest{
				Message: msg,
				Config: &a2a.SendMessageConfig{
					ReturnImmediately:   !req.GetBool("blocking", false),
					AcceptedOutputModes: []string{TextMediaType},
				},
			})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			metrics.IncMessagesSent()
			// Регистрируем исходящую задачу — чтобы когда пир ответит, ответ
			// автоматически попал в inbox и hook подсунул его пользователю
			// в следующий turn.
			if task, ok := res.(*a2a.Task); ok {
				d.Store.TrackOutgoing(string(task.ID), peerURL, client.Card().Name, redacted)
				if task.Status.State.Terminal() {
					d.Store.IngestOutgoingTerminal(task)
				} else {
					// Subscribe on the peer so the reply lands the moment the
					// peer reaches a terminal state — no need to wait for the
					// 5-second polling tick, which stays as a safety net.
					go subscribeOutgoingReply(d.lifetime(), d.Peers, peerURL, task.ID, d.Store, d.logger())
				}
			}
			return jsonResult(res)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_send_streaming",
			mcp.WithDescription("Call SendStreamingMessage and wait until the task reaches a terminal state. Returns the collected stream events."),
			mcp.WithString("peer_url", mcp.Required()),
			mcp.WithString("text", mcp.Required()),
			mcp.WithNumber("timeout_s", mcp.Description("Max seconds to wait (default 300)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, err := req.RequireString("peer_url")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			timeout := defaultStreamingTimeout
			if v := req.GetFloat("timeout_s", 0); v > 0 {
				timeout = time.Duration(v * float64(time.Second))
			}

			// Same secret screen as a2a_send_message — streaming must not
			// be a side door for unredacted text.
			redacted, hits := screenOutbound(text, d.logger())
			msg := d.outboundMessage(redacted, hits)

			sctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			client, err := d.Peers.Client(sctx, peerURL)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			defer d.Peers.Release(client)

			collected := []a2a.StreamResponse{}
			var streamErr error
			for ev, err := range client.SendStreamingMessage(sctx, &a2a.SendMessageRequest{
				Message: msg,
				Config:  &a2a.SendMessageConfig{AcceptedOutputModes: []string{TextMediaType}},
			}) {
				if err != nil {
					streamErr = err
					break
				}
				collected = append(collected, a2a.StreamResponse{Event: ev})
				if isTerminalEvent(ev) {
					break
				}
			}
			if streamErr != nil {
				if ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded) {
					// The peer accepted the message and is still working —
					// don't discard what we already streamed; surface it
					// with an explicit note.
					metrics.IncMessagesSent()
					b, err := json.MarshalIndent(collected, "", "  ")
					if err != nil {
						return mcp.NewToolResultError("encode result: " + err.Error()), nil
					}
					return mcp.NewToolResultText(fmt.Sprintf(
						"note: timed out after %s before the task reached a terminal state; events collected so far:\n%s",
						timeout, b)), nil
				}
				return mcp.NewToolResultError(streamErr.Error()), nil
			}
			metrics.IncMessagesSent()
			return jsonResult(collected)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_get_task",
			mcp.WithDescription("Call GetTask on a peer."),
			mcp.WithString("peer_url", mcp.Required()),
			mcp.WithString("task_id", mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, err := req.RequireString("peer_url")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			rctx, cancel := context.WithTimeout(ctx, peerRequestTimeout)
			defer cancel()
			t, err := d.Peers.GetTask(rctx, peerURL, a2a.TaskID(taskID))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(t)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_cancel_task",
			mcp.WithDescription("Call CancelTask on a peer."),
			mcp.WithString("peer_url", mcp.Required()),
			mcp.WithString("task_id", mcp.Required()),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			peerURL, err := req.RequireString("peer_url")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			rctx, cancel := context.WithTimeout(ctx, peerRequestTimeout)
			defer cancel()
			client, err := d.Peers.Client(rctx, peerURL)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			defer d.Peers.Release(client)
			t, err := client.CancelTask(rctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(taskID)})
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			return jsonResult(t)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_inbox",
			mcp.WithDescription("Drain or peek pending messages that peers have sent to this agent. Each entry includes taskId so you can a2a_complete_task after answering."),
			mcp.WithBoolean("peek", mcp.Description("If true, read without clearing")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var entries []InboxEntry
			if req.GetBool("peek", false) {
				entries = d.Store.PeekInbox()
			} else {
				entries = d.Store.DrainInbox()
			}
			if len(entries) == 0 {
				return mcp.NewToolResultText("[]"), nil
			}
			return jsonResult(entries)
		},
	)

	s.AddTool(
		mcp.NewTool("a2a_complete_task",
			mcp.WithDescription("Attach a reply as an Artifact and mark the local task COMPLETED. Use this to answer a peer's incoming message after processing it."),
			mcp.WithString("task_id", mcp.Required()),
			mcp.WithString("text", mcp.Required(), mcp.Description("Reply body")),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			taskID, err := req.RequireString("task_id")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			text, err := req.RequireString("text")
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			// The reply leaves this agent via streams / push webhooks, so it
			// goes through the same secret screen as direct sends.
			redacted, hits := screenOutbound(text, d.logger())
			if err := d.Executor.CompleteTask(taskID, redacted); err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			metrics.IncMessagesSent()
			if len(hits) > 0 {
				return mcp.NewToolResultText("completed (" + security.FormatMatches(hits) + ")"), nil
			}
			return mcp.NewToolResultText("completed"), nil
		},
	)
}

// isTerminalEvent reports whether a stream event ends the task's lifecycle.
func isTerminalEvent(ev a2a.Event) bool {
	switch v := ev.(type) {
	case *a2a.Message:
		return true
	case *a2a.Task:
		return v.Status.State.Terminal()
	case *a2a.TaskStatusUpdateEvent:
		return v.Status.State.Terminal()
	default:
		return false
	}
}

// PeerInfo combines directory entry + fetched Agent Card.
type PeerInfo struct {
	URL  string         `json:"url"`
	Card *a2a.AgentCard `json:"card,omitempty"`
	Err  string         `json:"error,omitempty"`
}

func listPeers(ctx context.Context, peers *Peers, directoryURL, selfURL string) ([]PeerInfo, error) {
	if directoryURL == "" {
		return nil, errors.New("A2A_DIRECTORY not set")
	}
	dirCtx, cancel := context.WithTimeout(ctx, directoryRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dirCtx, http.MethodGet, strings.TrimRight(directoryURL, "/")+"/agents", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("directory request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("directory returned %s", resp.Status)
	}
	var entries []struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}
	urls := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.URL == selfURL {
			continue
		}
		urls = append(urls, e.URL)
	}
	// Fetch agent cards concurrently with a bounded fan-out — sequential
	// fetches make one hung peer stall the whole discovery call.
	out := make([]PeerInfo, len(urls))
	sem := make(chan struct{}, peerCardFanOut)
	var wg sync.WaitGroup
	for i, peerURL := range urls {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			info := PeerInfo{URL: peerURL}
			cardCtx, ccancel := context.WithTimeout(ctx, directoryRequestTimeout)
			defer ccancel()
			card, err := peers.Resolve(cardCtx, peerURL)
			if err != nil {
				info.Err = err.Error()
			} else {
				info.Card = card
			}
			out[i] = info
		})
	}
	wg.Wait()
	return out, nil
}

// Heartbeat periodically re-registers this agent with the directory.
// Every POST carries its own timeout so a wedged directory can't park
// this goroutine on a response that never comes.
func Heartbeat(ctx context.Context, directoryURL, selfURL string) {
	body, _ := json.Marshal(map[string]string{"url": selfURL})
	do := func(reqCtx context.Context, path string, timeout time.Duration) {
		rctx, cancel := context.WithTimeout(reqCtx, timeout)
		defer cancel()
		req, _ := http.NewRequestWithContext(rctx, http.MethodPost,
			strings.TrimRight(directoryURL, "/")+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	do(ctx, "/register", directoryRequestTimeout)
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// The outer ctx is already canceled — a request built on it
			// would abort instantly and the directory would keep listing a
			// dead bridge for a full TTL. Use a short fresh context.
			do(context.Background(), "/unregister", 2*time.Second)
			return
		case <-t.C:
			do(ctx, "/heartbeat", directoryRequestTimeout)
		}
	}
}

// subscribeOutgoingReply opens a SubscribeToTask stream on the peer for the
// just-created outbound task. When the stream reports a terminal state — or
// ends for any other reason, e.g. the task finished before we subscribed —
// the full Task is resolved via GetTask and handed to
// Store.IngestOutgoingTerminal, which ignores non-terminal tasks. That drops
// a synthetic reply into our inbox without waiting for the 5-second polling
// fallback in internal/cli/bridge.go, which still catches anything missed here.
// Both steps derive from lifetime, so a bridge shutdown stops them.
func subscribeOutgoingReply(lifetime context.Context, peers *Peers, peerURL string, taskID a2a.TaskID, store *Store, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(lifetime, outgoingSubscribeTimeout)
	defer cancel()

	client, err := peers.Client(ctx, peerURL)
	if err != nil {
		log.Debug("outgoing reply subscription not opened", "task", taskID, "peer", peerURL, "err", err)
		return
	}
	defer peers.Release(client)

	for ev, err := range client.SubscribeToTask(ctx, &a2a.SubscribeToTaskRequest{ID: taskID}) {
		if err != nil {
			log.Debug("outgoing reply subscription ended", "task", taskID, "peer", peerURL, "err", err)
			break
		}
		if isTerminalEvent(ev) {
			break
		}
	}

	if lifetime.Err() != nil {
		return
	}
	// Status updates carry the state but not the artifacts; the synthesized
	// reply needs the artifact text, so resolve the full task. The subscription
	// timeout may have expired, so this call gets its own budget.
	rctx, rcancel := context.WithTimeout(lifetime, outgoingResolveTimeout)
	defer rcancel()
	task, err := client.GetTask(rctx, &a2a.GetTaskRequest{ID: taskID})
	if err != nil {
		log.Debug("outgoing task resolve failed", "task", taskID, "peer", peerURL, "err", err)
		return
	}
	store.IngestOutgoingTerminal(task)
}
