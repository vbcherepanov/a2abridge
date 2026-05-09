// Package agent contains the local A2A Handler implementation used by the bridge.
// It holds an in-memory task store and an inbox of messages addressed to this agent.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vbcherepanov/a2abridge/internal/a2a"
)

// Store implements a2a.Handler for a local agent.
// Incoming SendMessage calls create a task in SUBMITTED state and push the message
// onto the inbox so the host (Claude/Codex) can pick it up via MCP tools and reply.
type Store struct {
	mu          sync.Mutex
	tasks       map[string]*a2a.Task
	subscribers map[string][]chan a2a.StreamResponse
	inbox       []a2a.Message // incoming messages awaiting host handling

	// InboxPath — optional file path. Whenever the inbox changes the store
	// writes a JSON snapshot there so external hooks (UserPromptSubmit) can
	// read pending messages without going through MCP.
	InboxPath string

	// OnIncoming — optional async hook fired after a message is appended to inbox.
	// Used by the autonomous responder to spawn `claude -p` / `codex exec`.
	OnIncoming func(a2a.Message)

	// Outgoing task tracking: когда этот агент отправляет сообщение пиру,
	// мы запоминаем task_id + peer_url и фоново опрашиваем пока не COMPLETED.
	// Когда пришёл ответ — синтетическое сообщение кладётся в inbox, чтобы
	// UserPromptSubmit hook его инжектнул в следующий промпт пользователя.
	pendingOutgoing map[string]*pendingOutgoingTask

	// Push — webhook registry per A2A 1.0 §9.5. Bridges register peer
	// webhooks here; Store calls Notify on every task state change so
	// subscribers without an open SSE stream still see updates.
	Push *PushStore
}

// CreatePushConfig / GetPushConfig / ListPushConfigs / DeletePushConfig:
// thin pass-throughs that turn Store into an a2a.PushHandler. We forward
// to the embedded *PushStore so the JSON-RPC dispatcher in
// internal/a2a/server.go finds these methods on the Handler value the
// bridge already wires up.

func (s *Store) CreatePushConfig(ctx context.Context, in a2a.TaskPushNotificationConfig) (*a2a.TaskPushNotificationConfig, error) {
	return s.Push.CreatePushConfig(ctx, in)
}
func (s *Store) GetPushConfig(ctx context.Context, in a2a.PushNotificationConfigParams) (*a2a.TaskPushNotificationConfig, error) {
	return s.Push.GetPushConfig(ctx, in)
}
func (s *Store) ListPushConfigs(ctx context.Context, taskID string) ([]a2a.TaskPushNotificationConfig, error) {
	return s.Push.ListPushConfigs(ctx, taskID)
}
func (s *Store) DeletePushConfig(ctx context.Context, in a2a.PushNotificationConfigParams) error {
	return s.Push.DeletePushConfig(ctx, in)
}

type pendingOutgoingTask struct {
	TaskID   string
	PeerURL  string
	Question string
	PeerName string
	SentAt   time.Time
}

func NewStore() *Store {
	return &Store{
		tasks:           map[string]*a2a.Task{},
		subscribers:     map[string][]chan a2a.StreamResponse{},
		pendingOutgoing: map[string]*pendingOutgoingTask{},
		Push:            NewPushStore(),
	}
}

// TrackOutgoing registers a task initiated by this agent for background polling.
func (s *Store) TrackOutgoing(taskID, peerURL, peerName, question string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingOutgoing[taskID] = &pendingOutgoingTask{
		TaskID: taskID, PeerURL: peerURL, PeerName: peerName,
		Question: question, SentAt: time.Now(),
	}
}

// IngestOutgoingTerminal is the SSE-fast-path equivalent of PollOutgoing.
// Bridges open a SubscribeToTask SSE stream to each peer right after
// TrackOutgoing; when an a2a.SubscribeToTask event arrives with a
// terminal state, the bridge passes the resolved Task here so the inbox
// gets the reply with sub-second latency instead of waiting for the next
// 5-second polling tick.
//
// Idempotent: a Task ID that has already been delivered (or never tracked)
// is silently dropped — the polling fallback won't re-queue it.
func (s *Store) IngestOutgoingTerminal(t *a2a.Task) bool {
	if t == nil {
		return false
	}
	switch t.Status.State {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed,
		a2a.TaskStateCanceled, a2a.TaskStateRejected:
	default:
		return false
	}
	s.mu.Lock()
	p, ok := s.pendingOutgoing[t.ID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.pendingOutgoing, t.ID)
	s.mu.Unlock()

	reply := extractReplyText(t)
	s.appendSyntheticReply(p, reply, string(t.Status.State))
	return true
}

// extractReplyText pulls the textual reply out of the peer's terminal
// task — first looking at any artifacts, then falling back to status.message.
// Shared between the polling and the SSE paths.
func extractReplyText(t *a2a.Task) string {
	reply := ""
	for _, a := range t.Artifacts {
		for _, pt := range a.Parts {
			if pt.Text == "" {
				continue
			}
			if reply != "" {
				reply += "\n"
			}
			reply += pt.Text
		}
	}
	if reply == "" && t.Status.Message != nil {
		for _, pt := range t.Status.Message.Parts {
			if pt.Text != "" {
				reply = pt.Text
				break
			}
		}
	}
	return reply
}

// appendSyntheticReply is the shared inbox-write step used by both the
// poll loop and the SSE fast-path. Holds the lock for as little time as
// possible and fires OnIncoming outside the critical section.
func (s *Store) appendSyntheticReply(p *pendingOutgoingTask, reply, state string) {
	synthetic := a2a.Message{
		MessageID: "reply-" + p.TaskID,
		TaskID:    p.TaskID,
		Role:      a2a.RoleAgent,
		Parts:     []a2a.Part{{Text: fmt.Sprintf("[ОТВЕТ от %s на твой вопрос «%s»]\n%s", p.PeerName, trimTo(p.Question, 80), reply)}},
		Metadata:  map[string]any{"from": p.PeerName, "kind": "outgoing-reply", "state": state},
	}
	s.mu.Lock()
	s.inbox = append(s.inbox, synthetic)
	s.persistInboxLocked()
	cb := s.OnIncoming
	s.mu.Unlock()
	if cb != nil {
		go cb(synthetic)
	}
	// Fire user hook with a flat payload so shell scripts can grep fields.
	FireHook("on-outgoing-reply", map[string]any{
		"taskId": p.TaskID,
		"from":   p.PeerName,
		"text":   reply,
		"state":  state,
	})
}

// PollOutgoing iterates pending outgoing tasks, queries each peer's GetTask,
// and when COMPLETED — synthesizes an inbox message with the reply so the
// UserPromptSubmit hook can inject it. Returns number of newly completed tasks.
// This is the fallback path for when the SSE subscription dies; under
// normal operation IngestOutgoingTerminal beats PollOutgoing to it.
func (s *Store) PollOutgoing(fetcher func(peerURL, taskID string) (*a2a.Task, error), maxAge time.Duration) int {
	s.mu.Lock()
	pending := make([]*pendingOutgoingTask, 0, len(s.pendingOutgoing))
	for _, p := range s.pendingOutgoing {
		pending = append(pending, p)
	}
	s.mu.Unlock()

	completed := 0
	for _, p := range pending {
		if time.Since(p.SentAt) > maxAge {
			s.mu.Lock()
			delete(s.pendingOutgoing, p.TaskID)
			s.mu.Unlock()
			continue
		}
		t, err := fetcher(p.PeerURL, p.TaskID)
		if err != nil {
			continue
		}
		switch t.Status.State {
		case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
			// извлечь ответ
			reply := ""
			for _, a := range t.Artifacts {
				for _, pt := range a.Parts {
					if pt.Text != "" {
						if reply != "" {
							reply += "\n"
						}
						reply += pt.Text
					}
				}
			}
			if reply == "" && t.Status.Message != nil {
				for _, pt := range t.Status.Message.Parts {
					if pt.Text != "" {
						reply = pt.Text
						break
					}
				}
			}
			s.mu.Lock()
			delete(s.pendingOutgoing, p.TaskID)
			synthetic := a2a.Message{
				MessageID: "reply-" + p.TaskID,
				TaskID:    p.TaskID,
				Role:      a2a.RoleAgent,
				Parts:     []a2a.Part{{Text: fmt.Sprintf("[ОТВЕТ от %s на твой вопрос «%s»]\n%s", p.PeerName, trimTo(p.Question, 80), reply)}},
				Metadata:  map[string]any{"from": p.PeerName, "kind": "outgoing-reply", "state": string(t.Status.State)},
			}
			s.inbox = append(s.inbox, synthetic)
			s.persistInboxLocked()
			cb := s.OnIncoming
			s.mu.Unlock()
			// Trigger nudger so the sender's live Claude/Codex gets a turn
			// to surface the reply — same path as for inbound messages.
			if cb != nil {
				go cb(synthetic)
			}
			completed++
		}
	}
	return completed
}

func trimTo(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// persistInboxLocked writes the current inbox to InboxPath atomically.
// Must be called with s.mu held.
func (s *Store) persistInboxLocked() {
	if s.InboxPath == "" {
		return
	}
	snap := make([]map[string]any, 0, len(s.inbox))
	for _, m := range s.inbox {
		text := ""
		for _, p := range m.Parts {
			if p.Text != "" {
				if text != "" {
					text += "\n"
				}
				text += p.Text
			}
		}
		from := ""
		if m.Metadata != nil {
			if v, ok := m.Metadata["from"].(string); ok {
				from = v
			}
		}
		snap = append(snap, map[string]any{
			"messageId": m.MessageID,
			"taskId":    m.TaskID,
			"contextId": m.ContextID,
			"from":      from,
			"text":      text,
		})
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := s.InboxPath + ".tmp"
	_ = os.WriteFile(tmp, b, 0644)
	_ = os.Rename(tmp, s.InboxPath)
}

// --- a2a.Handler ---

func (s *Store) SendMessage(ctx context.Context, p a2a.MessageSendParams) (*a2a.Task, *a2a.Message, error) {
	taskID := p.Message.TaskID
	if taskID == "" {
		taskID = uuid.NewString()
	}
	ctxID := p.Message.ContextID
	if ctxID == "" {
		ctxID = uuid.NewString()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	task, existed := s.tasks[taskID]
	if !existed {
		task = &a2a.Task{
			ID:        taskID,
			ContextID: ctxID,
			Kind:      "task",
			Status: a2a.TaskStatus{
				State:     a2a.TaskStateSubmitted,
				Timestamp: time.Now().UTC(),
			},
		}
		s.tasks[taskID] = task
	}
	msg := p.Message
	msg.TaskID = taskID
	msg.ContextID = ctxID
	if msg.MessageID == "" {
		msg.MessageID = uuid.NewString()
	}
	task.History = append(task.History, msg)
	s.inbox = append(s.inbox, msg)
	s.persistInboxLocked()

	if s.OnIncoming != nil {
		go s.OnIncoming(msg)
	}
	// Surface inbound messages to the user's hook directory so external
	// integrations (desktop notifications, Slack relay, audit log) get a
	// turn. The hook's payload mirrors the synthetic-reply shape so
	// scripts can be uniform across both events.
	from := ""
	if v, ok := msg.Metadata["from"].(string); ok {
		from = v
	}
	text := ""
	for _, pt := range msg.Parts {
		if pt.Text != "" {
			text = pt.Text
			break
		}
	}
	FireHook("on-inbound", map[string]any{
		"taskId": taskID,
		"from":   from,
		"text":   text,
	})

	s.notifyLocked(taskID, a2a.StreamResponse{
		StatusUpdate: &a2a.TaskStatusUpdateEvent{
			TaskID: taskID, ContextID: ctxID, Status: task.Status,
		},
	})

	cp := *task
	return &cp, nil, nil
}

func (s *Store) GetTask(ctx context.Context, p a2a.TaskIDParams) (*a2a.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[p.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	cp := *t
	if p.HistoryLength > 0 && len(cp.History) > p.HistoryLength {
		cp.History = cp.History[len(cp.History)-p.HistoryLength:]
	}
	return &cp, nil
}

func (s *Store) CancelTask(ctx context.Context, p a2a.TaskIDParams) (*a2a.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[p.ID]
	if !ok {
		return nil, a2a.ErrTaskNotFound
	}
	if isTerminal(t.Status.State) {
		return nil, errors.New("task is in terminal state")
	}
	t.Status = a2a.TaskStatus{State: a2a.TaskStateCanceled, Timestamp: time.Now().UTC()}
	s.notifyLocked(t.ID, a2a.StreamResponse{
		StatusUpdate: &a2a.TaskStatusUpdateEvent{
			TaskID: t.ID, ContextID: t.ContextID, Status: t.Status, Final: true,
		},
	})
	cp := *t
	return &cp, nil
}

func (s *Store) ListTasks(ctx context.Context) ([]a2a.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]a2a.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, *t)
	}
	return out, nil
}

func (s *Store) Subscribe(ctx context.Context, id string, out chan<- a2a.StreamResponse) error {
	s.mu.Lock()
	t, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return a2a.ErrTaskNotFound
	}
	// send current snapshot
	snap := *t
	ch := make(chan a2a.StreamResponse, 8)
	s.subscribers[id] = append(s.subscribers[id], ch)
	s.mu.Unlock()

	out <- a2a.StreamResponse{Task: &snap}

	defer s.removeSubscriber(id, ch)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			out <- ev
			if ev.StatusUpdate != nil && ev.StatusUpdate.Final {
				return nil
			}
		}
	}
}

// StreamSend: same semantics as SendMessage, then streams until terminal.
func (s *Store) StreamSend(ctx context.Context, p a2a.MessageSendParams, out chan<- a2a.StreamResponse) error {
	task, _, err := s.SendMessage(ctx, p)
	if err != nil {
		return err
	}
	out <- a2a.StreamResponse{Task: task}
	return s.Subscribe(ctx, task.ID, out)
}

// --- host-facing API (used by MCP tools) ---

// DrainInbox returns and clears pending incoming messages.
func (s *Store) DrainInbox() []a2a.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.inbox
	s.inbox = nil
	s.persistInboxLocked()
	return out
}

// PeekInbox returns without clearing.
func (s *Store) PeekInbox() []a2a.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]a2a.Message, len(s.inbox))
	copy(out, s.inbox)
	return out
}

// CompleteTask attaches an agent reply as history + final artifact and transitions to COMPLETED.
// It also drops any inbox entries tied to the same task so the hook-based summary stops mentioning it.
func (s *Store) CompleteTask(taskID, replyText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[taskID]
	if !ok {
		return a2a.ErrTaskNotFound
	}
	// drop inbox entries for this task
	filtered := s.inbox[:0]
	for _, m := range s.inbox {
		if m.TaskID != taskID {
			filtered = append(filtered, m)
		}
	}
	s.inbox = filtered
	s.persistInboxLocked()
	reply := a2a.Message{
		MessageID: uuid.NewString(),
		ContextID: t.ContextID,
		TaskID:    t.ID,
		Role:      a2a.RoleAgent,
		Parts:     []a2a.Part{{Text: replyText}},
	}
	t.History = append(t.History, reply)
	t.Artifacts = append(t.Artifacts, a2a.Artifact{
		ArtifactID: uuid.NewString(),
		Name:       "reply",
		Parts:      []a2a.Part{{Text: replyText}},
	})
	t.Status = a2a.TaskStatus{
		State:     a2a.TaskStateCompleted,
		Message:   &reply,
		Timestamp: time.Now().UTC(),
	}
	s.notifyLocked(t.ID, a2a.StreamResponse{
		ArtifactUpdate: &a2a.TaskArtifactUpdateEvent{
			TaskID: t.ID, ContextID: t.ContextID,
			Artifact:  t.Artifacts[len(t.Artifacts)-1],
			LastChunk: true,
		},
	})
	s.notifyLocked(t.ID, a2a.StreamResponse{
		StatusUpdate: &a2a.TaskStatusUpdateEvent{
			TaskID: t.ID, ContextID: t.ContextID, Status: t.Status, Final: true,
		},
	})
	return nil
}

func (s *Store) notifyLocked(taskID string, ev a2a.StreamResponse) {
	for _, ch := range s.subscribers[taskID] {
		select {
		case ch <- ev:
		default:
		}
	}
	// Webhook delivery is fire-and-forget so we can call it while holding
	// the lock — Notify only takes its own short lock to copy the config
	// snapshot before doing HTTP I/O in goroutines.
	if s.Push != nil {
		s.Push.Notify(taskID, ev)
	}
}

func (s *Store) removeSubscriber(taskID string, ch chan a2a.StreamResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subs := s.subscribers[taskID]
	for i, c := range subs {
		if c == ch {
			s.subscribers[taskID] = append(subs[:i], subs[i+1:]...)
			close(c)
			return
		}
	}
}

func isTerminal(st a2a.TaskState) bool {
	switch st {
	case a2a.TaskStateCompleted, a2a.TaskStateFailed, a2a.TaskStateCanceled, a2a.TaskStateRejected:
		return true
	}
	return false
}
