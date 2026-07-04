// Package agent contains the local A2A Handler implementation used by the bridge.
// It holds an in-memory task store and an inbox of messages addressed to this agent.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vbcherepanov/a2abridge/internal/a2a"
	"github.com/vbcherepanov/a2abridge/internal/metrics"
)

// Janitor knobs. Terminal tasks are kept around for a grace period so
// late GetTask / resubscribe calls still resolve, then evicted to keep
// the in-memory store bounded on long-lived bridges.
const (
	terminalTaskTTL = 30 * time.Minute
	janitorInterval = time.Minute

	// inboxSoftCap bounds the persisted inbox. An inbox is normally drained
	// every turn; a soft cap keeps a stuck/never-drained bridge from growing
	// the snapshot without bound. Oldest entries are dropped past this.
	inboxSoftCap = 500
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

	// Log — optional structured logger for background failures (inbox
	// persistence, janitor). nil falls back to slog.Default().
	Log *slog.Logger

	janitorStop chan struct{}
	closeOnce   sync.Once
}

// logger returns the configured logger or the process default.
func (s *Store) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
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
	s := &Store{
		tasks:           map[string]*a2a.Task{},
		subscribers:     map[string][]chan a2a.StreamResponse{},
		pendingOutgoing: map[string]*pendingOutgoingTask{},
		Push:            NewPushStore(),
		janitorStop:     make(chan struct{}),
	}
	go s.janitor()
	return s
}

// Close stops the background janitor. Safe to call multiple times.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.janitorStop) })
}

// janitor periodically evicts terminal tasks older than terminalTaskTTL.
func (s *Store) janitor() {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.janitorStop:
			return
		case <-t.C:
			s.evictTerminal(time.Now())
		}
	}
}

// evictTerminal deletes tasks that reached a terminal state more than
// terminalTaskTTL ago and cascades the delete to their push configs.
// Non-terminal tasks are never evicted — they may still receive messages.
// Returns the number of evicted tasks.
func (s *Store) evictTerminal(now time.Time) int {
	cutoff := now.Add(-terminalTaskTTL)
	s.mu.Lock()
	var evicted []string
	for id, t := range s.tasks {
		if isTerminal(t.Status.State) && !t.Status.Timestamp.IsZero() && t.Status.Timestamp.Before(cutoff) {
			delete(s.tasks, id)
			evicted = append(evicted, id)
		}
	}
	s.mu.Unlock()
	for _, id := range evicted {
		// Empty PushConfigID = delete all webhooks for the task. The
		// ErrTaskNotFound case (no configs registered) is expected.
		_ = s.Push.DeletePushConfig(context.Background(), a2a.PushNotificationConfigParams{TaskID: id})
	}
	if len(evicted) > 0 {
		s.logger().Info("evicted terminal tasks", "count", len(evicted), "ttl", terminalTaskTTL)
	}
	return len(evicted)
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
	isNew := s.appendInboxLocked(synthetic)
	if isNew {
		s.persistInboxLocked()
	}
	cb := s.OnIncoming
	s.mu.Unlock()
	if !isNew {
		return // duplicate reply already queued (poll + SSE race) — don't re-inject or re-fire
	}
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
		if !isTerminal(t.Status.State) {
			continue
		}
		// Re-check membership under the lock: the SSE fast path
		// (IngestOutgoingTerminal) may have delivered this reply while we
		// were doing the network fetch above. Mirroring its idempotency
		// here prevents a double inbox entry.
		s.mu.Lock()
		if _, ok := s.pendingOutgoing[p.TaskID]; !ok {
			s.mu.Unlock()
			continue
		}
		delete(s.pendingOutgoing, p.TaskID)
		s.mu.Unlock()
		// Shared with the SSE path so OnIncoming and the
		// on-outgoing-reply user hook fire on both.
		s.appendSyntheticReply(p, extractReplyText(t), string(t.Status.State))
		completed++
	}
	return completed
}

// trimTo shortens s to at most n runes (not bytes — slicing bytes could
// split a multi-byte rune in half) and flattens newlines.
func trimTo(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// inboxContainsLocked reports whether a message with this id is already queued.
// Must be called with s.mu held.
func (s *Store) inboxContainsLocked(id string) bool {
	if id == "" {
		return false
	}
	for i := range s.inbox {
		if s.inbox[i].MessageID == id {
			return true
		}
	}
	return false
}

// appendInboxLocked adds a message to the inbox with effectively-once semantics
// (dedup by MessageID — a redelivery is dropped) and a soft cap (oldest dropped
// past inboxSoftCap). Returns true if the message was newly queued, false if it
// was a duplicate — callers use that to skip re-firing side-effects (hook,
// responder, metrics). Must be called with s.mu held.
func (s *Store) appendInboxLocked(m a2a.Message) bool {
	if s.inboxContainsLocked(m.MessageID) {
		return false
	}
	s.inbox = append(s.inbox, m)
	if over := len(s.inbox) - inboxSoftCap; over > 0 {
		s.logger().Warn("inbox soft-cap exceeded, dropping oldest", "cap", inboxSoftCap, "dropped", over)
		s.inbox = append([]a2a.Message(nil), s.inbox[over:]...)
	}
	return true
}

// LoadInbox repopulates the inbox from the on-disk snapshot at InboxPath,
// making delivery durable across a bridge restart: messages that arrived but
// were never drained survive a bounce instead of being lost. Call once at
// startup, after InboxPath is set. The snapshot is the flat hook-facing
// projection (persistInboxLocked), so reconstructed messages carry
// id/task/context/from/text — the fields the drain path + host actually use.
// Missing/unreadable/empty snapshot = a normal fresh start (no-op).
func (s *Store) LoadInbox() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.InboxPath == "" {
		return
	}
	b, err := os.ReadFile(s.InboxPath)
	if err != nil {
		return // no snapshot yet — fresh bridge
	}
	var snap []struct {
		MessageID string `json:"messageId"`
		TaskID    string `json:"taskId"`
		ContextID string `json:"contextId"`
		From      string `json:"from"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		s.logger().Warn("inbox snapshot load failed", "path", s.InboxPath, "err", err)
		return
	}
	for _, e := range snap {
		m := a2a.Message{
			MessageID: e.MessageID,
			TaskID:    e.TaskID,
			ContextID: e.ContextID,
			Role:      a2a.RoleUser,
			Parts:     []a2a.Part{{Text: e.Text}},
		}
		if e.From != "" {
			m.Metadata = map[string]any{"from": e.From}
		}
		s.appendInboxLocked(m)
	}
	metrics.SetInboxSize(len(s.inbox))
	if len(s.inbox) > 0 {
		s.logger().Info("inbox restored from snapshot", "count", len(s.inbox), "path", s.InboxPath)
	}
}

// persistInboxLocked writes the current inbox to InboxPath atomically.
// Must be called with s.mu held.
func (s *Store) persistInboxLocked() {
	metrics.SetInboxSize(len(s.inbox))
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
	// 0600 — the snapshot carries full inter-agent message text; other
	// local users have no business reading it.
	tmp := s.InboxPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		// Skip the rename: a stale tmp from a previous failure must not
		// be promoted over the last good snapshot.
		s.logger().Warn("inbox snapshot write failed", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, s.InboxPath); err != nil {
		s.logger().Warn("inbox snapshot rename failed", "path", s.InboxPath, "err", err)
	}
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
	if existed && isTerminal(task.Status.State) {
		// A terminal task accepts no further input — appending would only
		// grow history and re-notify subscribers with a stale final status.
		return nil, nil, fmt.Errorf("task %s is in terminal state %s and accepts no further messages: %w", taskID, task.Status.State, a2a.ErrTaskNotCancelable)
	}
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
	// Effectively-once: a redelivered message (same MessageID) is not
	// re-queued, and its side-effects (history, hook, responder, metrics)
	// are not re-fired. The sender still gets a valid task back.
	isNew := s.appendInboxLocked(msg)
	if isNew {
		task.History = append(task.History, msg)
		s.persistInboxLocked()
		metrics.IncMessagesReceived()

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
	}

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
	metrics.IncTaskFailed()
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
	if isTerminal(snap.Status.State) {
		// No further events will ever arrive — deliver the snapshot and
		// end the stream instead of parking a goroutine forever.
		s.mu.Unlock()
		select {
		case out <- a2a.StreamResponse{Task: &snap}:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}
	ch := make(chan a2a.StreamResponse, 8)
	s.subscribers[id] = append(s.subscribers[id], ch)
	s.mu.Unlock()

	defer s.removeSubscriber(id, ch)

	// Every send to out is ctx-guarded: if the consumer stops reading we
	// must not block forever and leak this goroutine.
	select {
	case out <- a2a.StreamResponse{Task: &snap}:
	case <-ctx.Done():
		return ctx.Err()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
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
	metrics.IncTaskCompleted()
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
	final := ev.StatusUpdate != nil && ev.StatusUpdate.Final
	for _, ch := range s.subscribers[taskID] {
		select {
		case ch <- ev:
		default:
			// Buffer full. Intermediate events may be dropped under
			// backpressure, but a terminal (Final) event must reach the
			// subscriber: evict the oldest buffered event to make room.
			// notifyLocked is the only sender and always runs under s.mu,
			// so after the eviction the second send cannot fail.
			if !final {
				continue
			}
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
	if final {
		// The stream is over: close every subscriber channel so consumers
		// drain what's buffered and exit, and drop the bookkeeping entry
		// (removeSubscriber tolerates already-removed channels).
		for _, ch := range s.subscribers[taskID] {
			close(ch)
		}
		delete(s.subscribers, taskID)
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
