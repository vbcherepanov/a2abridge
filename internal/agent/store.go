// Package agent contains the local A2A agent used by the bridge: the durable
// inbox the host (Claude/Codex) drains through MCP tools, outgoing task
// tracking, the a2a-go executor and the HTTP assembly around it.
package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/vbcherepanov/a2abridge/v4/internal/metrics"
)

// Janitor knobs.
const (
	janitorInterval = time.Minute

	// inboxSoftCap bounds the persisted inbox. An inbox is normally drained
	// every turn; a soft cap keeps a stuck/never-drained bridge from growing
	// the snapshot without bound. Oldest entries are dropped past this.
	inboxSoftCap = 500

	// replyEntryTTL is how long a delivered one-shot outgoing-reply entry
	// stays in the inbox before the janitor reaps it.
	replyEntryTTL = 2 * time.Minute

	// replyMessageIDPrefix marks synthetic outgoing-reply entries. The prefix
	// survives the on-disk snapshot, so it is the identity used after a restart.
	replyMessageIDPrefix = "reply-"

	// KindOutgoingReply is the InboxEntry.Kind of a synthetic reply to a task
	// this agent sent to a peer.
	KindOutgoingReply = "outgoing-reply"

	// inboxFileMode — the snapshot carries full inter-agent message text;
	// other local users have no business reading it.
	inboxFileMode = 0o600
)

// InboxEntry is one message waiting for the host: either a peer's inbound
// task or a synthetic reply to a task this agent sent.
type InboxEntry struct {
	MessageID string `json:"messageId"`
	TaskID    string `json:"taskId"`
	ContextID string `json:"contextId"`
	From      string `json:"from"`
	Text      string `json:"text"`
	Kind      string `json:"kind,omitempty"`
	State     string `json:"state,omitempty"`
	TS        string `json:"ts,omitempty"`
}

// isSyntheticReply reports whether the entry is a synthetic outgoing-reply
// the store injected for one of our own outbound tasks.
func (e *InboxEntry) isSyntheticReply() bool {
	return e.Kind == KindOutgoingReply || strings.HasPrefix(e.MessageID, replyMessageIDPrefix)
}

// snapshotEntry is the hook-facing on-disk projection of an InboxEntry.
// internal/assets/hook/a2a-inbox-hook.sh reads exactly these fields.
type snapshotEntry struct {
	MessageID string `json:"messageId"`
	TaskID    string `json:"taskId"`
	ContextID string `json:"contextId"`
	From      string `json:"from"`
	Text      string `json:"text"`
	TS        string `json:"ts,omitempty"`
}

// Store holds the durable inbox of messages addressed to this agent and the
// outgoing tasks this agent is waiting on. Task state itself lives in the
// a2a-go task store (TTLStore); the Store only knows what the host sees.
type Store struct {
	mu    sync.Mutex
	inbox []InboxEntry

	// InboxPath — optional file path. Whenever the inbox changes the store
	// writes a JSON snapshot there so external hooks (UserPromptSubmit) can
	// read pending messages without going through MCP.
	InboxPath string

	// OnIncoming — optional async hook fired after an entry is appended to inbox.
	// Used by the autonomous responder to spawn `claude -p` / `codex exec`.
	OnIncoming func(*InboxEntry)

	// Outgoing task tracking: когда этот агент отправляет сообщение пиру,
	// мы запоминаем task_id + peer_url и фоново опрашиваем пока не COMPLETED.
	// Когда пришёл ответ — синтетическое сообщение кладётся в inbox, чтобы
	// UserPromptSubmit hook его инжектнул в следующий промпт пользователя.
	pendingOutgoing map[string]*pendingOutgoingTask

	// Log — optional structured logger for background failures (inbox
	// persistence, janitor). nil falls back to slog.Default().
	Log *slog.Logger

	janitorStop chan struct{}
	closeOnce   sync.Once
}

type pendingOutgoingTask struct {
	TaskID   string
	PeerURL  string
	Question string
	PeerName string
	SentAt   time.Time
}

// NewStore returns an empty store with its reply janitor running.
func NewStore() *Store {
	s := &Store{
		pendingOutgoing: map[string]*pendingOutgoingTask{},
		janitorStop:     make(chan struct{}),
	}
	go s.janitor()
	return s
}

// logger returns the configured logger or the process default.
func (s *Store) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Close stops the background janitor. Safe to call multiple times.
func (s *Store) Close() {
	s.closeOnce.Do(func() { close(s.janitorStop) })
}

// janitor periodically reaps delivered outgoing-reply entries.
func (s *Store) janitor() {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-s.janitorStop:
			return
		case <-t.C:
			s.evictExpiredReplies(time.Now())
		}
	}
}

// evictExpiredReplies drops outgoing-reply entries delivered more than
// replyEntryTTL ago and returns how many were dropped.
//
// FIX(stale-re-render): outgoing-reply notifications (MessageID "reply-…") are
// one-shot — delivered in real-time via OnIncoming + injected by the wake hook.
// The bot never complete_task's its OWN outgoing task IDs, so CompleteTask's
// by-taskID drop never reaches them and they re-rendered on every wake. Evict
// by IDENTITY (MessageID prefix; `kind` is stripped on-disk) once past the
// delivery TTL.
func (s *Store) evictExpiredReplies(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inbox) == 0 {
		return 0
	}
	cutoff := now.Add(-replyEntryTTL)
	kept := s.inbox[:0]
	for i := range s.inbox {
		e := &s.inbox[i]
		if strings.HasPrefix(e.MessageID, replyMessageIDPrefix) && e.TS != "" {
			if when, err := time.Parse(time.RFC3339, e.TS); err == nil && when.Before(cutoff) {
				continue // delivered + aged out → drop
			}
		}
		kept = append(kept, *e)
	}
	dropped := len(s.inbox) - len(kept)
	if dropped > 0 {
		s.inbox = kept
		s.persistInboxLocked()
	}
	return dropped
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

// IngestOutgoingTerminal is the fast-path equivalent of PollOutgoing. The
// bridge opens a SubscribeToTask stream to the peer right after TrackOutgoing
// (or already holds a terminal task from a blocking send); the resolved Task
// lands here so the inbox gets the reply with sub-second latency instead of
// waiting for the next 5-second polling tick.
//
// Idempotent: a Task ID that has already been delivered (or never tracked)
// is dropped — the polling fallback won't re-queue it.
func (s *Store) IngestOutgoingTerminal(t *a2a.Task) bool {
	if t == nil || !t.Status.State.Terminal() {
		return false
	}
	s.mu.Lock()
	p, ok := s.pendingOutgoing[string(t.ID)]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.pendingOutgoing, string(t.ID))
	s.mu.Unlock()

	s.appendSyntheticReply(p, extractReplyText(t), string(t.Status.State))
	return true
}

// extractReplyText pulls the textual reply out of the peer's terminal
// task — first looking at any artifacts, then falling back to status.message.
// Shared between the polling and the subscription paths.
func extractReplyText(t *a2a.Task) string {
	var b strings.Builder
	for _, a := range t.Artifacts {
		if a == nil {
			continue
		}
		for _, pt := range a.Parts {
			text := partText(pt)
			if text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
	}
	if b.Len() == 0 && t.Status.Message != nil {
		for _, pt := range t.Status.Message.Parts {
			if text := partText(pt); text != "" {
				return text
			}
		}
	}
	return b.String()
}

// partText returns the text content of a part, or "" for nil / non-text parts.
func partText(p *a2a.Part) string {
	if p == nil {
		return ""
	}
	return p.Text()
}

// messageText concatenates the text parts of a message with newlines.
func messageText(m *a2a.Message) string {
	var b strings.Builder
	for _, pt := range m.Parts {
		text := partText(pt)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// appendSyntheticReply is the shared inbox-write step used by both the
// poll loop and the subscription fast-path. Holds the lock for as little time
// as possible and fires OnIncoming outside the critical section.
func (s *Store) appendSyntheticReply(p *pendingOutgoingTask, reply, state string) {
	// FIX(empty-arrivals): a contentless completion (peer completed with no reply
	// text — e.g. a bare a2a_complete_task ack) needs no inbox entry. The terminal
	// state is already recorded on the task, and an empty "[ОТВЕТ …]" record is just
	// noise that would feed the never-cleared stale floor — so skip it.
	if strings.TrimSpace(reply) == "" {
		return
	}
	synthetic := InboxEntry{
		MessageID: replyMessageIDPrefix + p.TaskID,
		TaskID:    p.TaskID,
		From:      p.PeerName,
		Text:      fmt.Sprintf("[ОТВЕТ от %s на твой вопрос «%s»]\n%s", p.PeerName, trimTo(p.Question, 80), reply),
		Kind:      KindOutgoingReply,
		State:     state,
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
	s.mu.Lock()
	isNew := s.appendInboxLocked(&synthetic)
	if isNew {
		s.persistInboxLocked()
	}
	cb := s.OnIncoming
	s.mu.Unlock()
	if !isNew {
		return // duplicate reply already queued (poll + stream race) — don't re-inject or re-fire
	}
	if cb != nil {
		go cb(&synthetic)
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
// and when terminal — synthesizes an inbox entry with the reply so the
// UserPromptSubmit hook can inject it. Returns number of newly completed tasks.
// This is the fallback path for when the subscription dies; under
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
			s.logger().Debug("outgoing task poll failed", "task", p.TaskID, "peer", p.PeerURL, "err", err)
			continue
		}
		if t == nil || !t.Status.State.Terminal() {
			continue
		}
		// Re-check membership under the lock: the fast path
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
		// Shared with the subscription path so OnIncoming and the
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

// deliverIncoming queues a peer's inbound message for the host. Returns false
// when an entry with the same MessageID is already queued; side-effects
// (snapshot, metrics, OnIncoming, on-inbound hook) fire only for new entries.
func (s *Store) deliverIncoming(e *InboxEntry) bool {
	s.mu.Lock()
	isNew := s.appendInboxLocked(e)
	if isNew {
		s.persistInboxLocked()
	}
	cb := s.OnIncoming
	s.mu.Unlock()
	if !isNew {
		return false
	}
	metrics.IncMessagesReceived()
	if cb != nil {
		// The callback gets its own copy; the caller keeps e.
		entry := *e
		go cb(&entry)
	}
	// Surface inbound messages to the user's hook directory so external
	// integrations (desktop notifications, Slack relay, audit log) get a
	// turn. The hook's payload mirrors the synthetic-reply shape so
	// scripts can be uniform across both events.
	FireHook("on-inbound", map[string]any{
		"taskId": e.TaskID,
		"from":   e.From,
		"text":   e.Text,
	})
	return true
}

// inboxContainsLocked reports whether an entry with this id is already queued.
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

// appendInboxLocked adds an entry to the inbox with effectively-once semantics
// (dedup by MessageID — a redelivery is dropped) and a soft cap (oldest dropped
// past inboxSoftCap). Returns true if the entry was newly queued, false if it
// was a duplicate — callers use that to skip re-firing side-effects (hook,
// responder, metrics). Must be called with s.mu held.
func (s *Store) appendInboxLocked(e *InboxEntry) bool {
	if s.inboxContainsLocked(e.MessageID) {
		return false
	}
	s.inbox = append(s.inbox, *e)
	if over := len(s.inbox) - inboxSoftCap; over > 0 {
		s.logger().Warn("inbox soft-cap exceeded, dropping oldest", "cap", inboxSoftCap, "dropped", over)
		s.inbox = append([]InboxEntry(nil), s.inbox[over:]...)
	}
	return true
}

// LoadInbox repopulates the inbox from the on-disk snapshot at InboxPath,
// making delivery durable across a bridge restart: messages that arrived but
// were never drained survive a bounce instead of being lost. Call once at
// startup, after InboxPath is set. The snapshot is the flat hook-facing
// projection (persistInboxLocked), so restored entries carry
// id/task/context/from/text/ts — the fields the drain path + host actually use.
// Missing/unreadable/empty snapshot = a normal fresh start (no-op).
func (s *Store) LoadInbox() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.InboxPath == "" {
		return
	}
	b, err := os.ReadFile(s.InboxPath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger().Warn("inbox snapshot read failed", "path", s.InboxPath, "err", err)
		}
		return
	}
	if len(b) == 0 {
		return // the hook truncates the file after rendering it
	}
	var snap []snapshotEntry
	if err := json.Unmarshal(b, &snap); err != nil {
		s.logger().Warn("inbox snapshot load failed", "path", s.InboxPath, "err", err)
		return
	}
	for i := range snap {
		e := InboxEntry{
			MessageID: snap[i].MessageID,
			TaskID:    snap[i].TaskID,
			ContextID: snap[i].ContextID,
			From:      snap[i].From,
			Text:      snap[i].Text,
			TS:        snap[i].TS,
		}
		s.appendInboxLocked(&e)
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
	snap := make([]snapshotEntry, 0, len(s.inbox))
	for i := range s.inbox {
		e := &s.inbox[i]
		// The delivery timestamp lets the janitor age out one-shot
		// outgoing-reply records restored after a bridge restart.
		snap = append(snap, snapshotEntry{
			MessageID: e.MessageID,
			TaskID:    e.TaskID,
			ContextID: e.ContextID,
			From:      e.From,
			Text:      e.Text,
			TS:        e.TS,
		})
	}
	b, err := json.Marshal(snap)
	if err != nil {
		s.logger().Warn("inbox snapshot encode failed", "path", s.InboxPath, "err", err)
		return
	}
	tmp := s.InboxPath + ".tmp"
	if err := os.WriteFile(tmp, b, inboxFileMode); err != nil {
		// Skip the rename: a stale tmp from a previous failure must not
		// be promoted over the last good snapshot.
		s.logger().Warn("inbox snapshot write failed", "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, s.InboxPath); err != nil {
		s.logger().Warn("inbox snapshot rename failed", "path", s.InboxPath, "err", err)
	}
}

// --- host-facing API (used by MCP tools) ---

// DrainInbox returns and clears pending incoming entries.
func (s *Store) DrainInbox() []InboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.inbox
	s.inbox = nil
	s.persistInboxLocked()
	return out
}

// PeekInbox returns pending entries without clearing genuine inbound ones.
func (s *Store) PeekInbox() []InboxEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]InboxEntry, len(s.inbox))
	copy(out, s.inbox)
	// FIX(wake-spam): outgoing-reply notifications (MessageID "reply-…") are
	// one-shot. A bot that PEEKs (instead of draining) would otherwise leave them
	// in s.inbox, re-surfacing them on every wake until the janitor TTL — the
	// residual wake-spam. Consume them on read: the caller gets them in `out`
	// this once, then they're gone (DrainInbox already clears all; this makes
	// peek consume the one-shot replies too). Genuine incoming task messages
	// are untouched, so peeking pending tasks stays non-destructive.
	kept := s.inbox[:0]
	dropped := false
	for i := range s.inbox {
		e := &s.inbox[i]
		if strings.HasPrefix(e.MessageID, replyMessageIDPrefix) {
			dropped = true
			continue
		}
		kept = append(kept, *e)
	}
	if dropped {
		s.inbox = kept
		s.persistInboxLocked()
	}
	return out
}

// dropTaskEntries removes every inbox entry tied to taskID so the hook-based
// summary stops mentioning it. It reports whether any removed entry was a
// synthetic outgoing-reply.
func (s *Store) dropTaskEntries(taskID string) (droppedReply bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.inbox[:0]
	dropped := false
	for i := range s.inbox {
		e := &s.inbox[i]
		if e.TaskID == taskID {
			dropped = true
			if e.isSyntheticReply() {
				droppedReply = true
			}
			continue
		}
		kept = append(kept, *e)
	}
	if dropped {
		s.inbox = kept
		s.persistInboxLocked()
	}
	return droppedReply
}
