package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func completedTask(id, reply string) *a2a.Task {
	return &a2a.Task{
		ID:        a2a.TaskID(id),
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{{Parts: a2a.ContentParts{a2a.NewTextPart(reply)}}},
	}
}

func incoming(id, taskID, text string) *InboxEntry {
	return &InboxEntry{MessageID: id, TaskID: taskID, ContextID: "ctx-" + taskID, From: "peer-a", Text: text}
}

// TestDeliverIncomingQueuesAndFires verifies the inbound path: the entry is
// queued and OnIncoming fires once.
func TestDeliverIncomingQueuesAndFires(t *testing.T) {
	s := NewStore()
	defer s.Close()
	var fired atomic.Int32
	s.OnIncoming = func(*InboxEntry) { fired.Add(1) }

	if !s.deliverIncoming(incoming("m1", "t1", "hello")) {
		t.Fatal("deliverIncoming = false for a new message")
	}
	pending := s.PeekInbox()
	if len(pending) != 1 || pending[0].MessageID != "m1" || pending[0].Text != "hello" {
		t.Fatalf("inbox = %+v, want the delivered entry", pending)
	}
	waitFor(t, "OnIncoming", func() bool { return fired.Load() == 1 })
}

// TestPollOutgoingInjectsReply verifies the asymmetric reply-injection
// path: when an outbound task we tracked completes on the peer's side,
// PollOutgoing must drop a synthetic entry into our inbox so the hook
// surfaces it on the next user prompt.
func TestPollOutgoingInjectsReply(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.TrackOutgoing("task-1", "http://peer/", "peer-A", "What is 2+2?")

	fetcher := func(_, taskID string) (*a2a.Task, error) {
		return completedTask(taskID, "4"), nil
	}

	if completed := s.PollOutgoing(fetcher, 10*time.Minute); completed != 1 {
		t.Fatalf("PollOutgoing returned %d, want 1", completed)
	}

	pending := s.PeekInbox()
	if len(pending) != 1 {
		t.Fatalf("inbox size = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.TaskID != "task-1" || got.Kind != KindOutgoingReply || got.State != string(a2a.TaskStateCompleted) {
		t.Errorf("synthetic entry = %+v", got)
	}
	if want := "[ОТВЕТ от peer-A на твой вопрос «What is 2+2?»]\n4"; got.Text != want {
		t.Errorf("synthetic text = %q, want %q", got.Text, want)
	}
}

// TestPollOutgoingDropsStaleTask ensures we don't grow pendingOutgoing
// indefinitely when peers never respond.
func TestPollOutgoingDropsStaleTask(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.TrackOutgoing("stale", "http://peer/", "peer", "?")
	s.mu.Lock()
	s.pendingOutgoing["stale"].SentAt = time.Now().Add(-1 * time.Hour)
	s.mu.Unlock()

	fetcher := func(_, _ string) (*a2a.Task, error) {
		t.Error("fetcher should not be called for stale tasks")
		return nil, errors.New("unexpected fetch")
	}
	if n := s.PollOutgoing(fetcher, 30*time.Minute); n != 0 {
		t.Errorf("PollOutgoing = %d, want 0", n)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pendingOutgoing["stale"]; ok {
		t.Errorf("stale task still in pendingOutgoing")
	}
}

// TestIngestOutgoingTerminalFastPath verifies that a terminal Task from the
// subscription fast-path produces the same inbox entry as the polling path —
// and removes the task from pendingOutgoing.
func TestIngestOutgoingTerminalFastPath(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.TrackOutgoing("task-sse", "http://peer/", "peer-A", "ping")

	if !s.IngestOutgoingTerminal(completedTask("task-sse", "pong")) {
		t.Fatal("IngestOutgoingTerminal should report delivered=true")
	}

	pending := s.PeekInbox()
	if len(pending) != 1 || pending[0].TaskID != "task-sse" {
		t.Fatalf("inbox = %+v, want one entry for task-sse", pending)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pendingOutgoing["task-sse"]; ok {
		t.Errorf("pendingOutgoing still contains task-sse after fast-path delivery")
	}
}

// TestIngestOutgoingTerminalIgnoresUntrackedAndLive drops Tasks that were
// never tracked or are not terminal yet.
func TestIngestOutgoingTerminalIgnoresUntrackedAndLive(t *testing.T) {
	s := NewStore()
	defer s.Close()
	if s.IngestOutgoingTerminal(completedTask("ghost", "boo")) {
		t.Error("delivered=true for an untracked task")
	}
	s.TrackOutgoing("live", "http://peer/", "peer-A", "q")
	if s.IngestOutgoingTerminal(&a2a.Task{ID: "live", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}) {
		t.Error("delivered=true for a working task")
	}
	if s.IngestOutgoingTerminal(nil) {
		t.Error("delivered=true for nil")
	}
	if len(s.PeekInbox()) != 0 {
		t.Error("inbox grew despite untracked / live tasks")
	}
}

// TestPollOutgoingSkipsAlreadyDelivered reproduces the stream-vs-poll race:
// the fast path delivers the reply while PollOutgoing is mid network fetch.
func TestPollOutgoingSkipsAlreadyDelivered(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.TrackOutgoing("task-race", "http://peer/", "peer-A", "ping")

	terminal := completedTask("task-race", "pong")
	fetcher := func(_, _ string) (*a2a.Task, error) {
		if !s.IngestOutgoingTerminal(terminal) {
			t.Error("fast path should have delivered")
		}
		return terminal, nil
	}

	if got := s.PollOutgoing(fetcher, 10*time.Minute); got != 0 {
		t.Errorf("PollOutgoing completed = %d, want 0 (fast path already delivered)", got)
	}
	if pending := s.PeekInbox(); len(pending) != 1 {
		t.Fatalf("inbox size = %d, want 1 (no double delivery)", len(pending))
	}
}

// TestExtractReplyTextFallsBackToStatusMessage: without artifacts the reply
// comes from the status message.
func TestExtractReplyTextFallsBackToStatusMessage(t *testing.T) {
	task := &a2a.Task{Status: a2a.TaskStatus{
		State:   a2a.TaskStateCompleted,
		Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("from status")),
	}}
	if got := extractReplyText(task); got != "from status" {
		t.Fatalf("extractReplyText = %q, want from status", got)
	}
	multi := &a2a.Task{Artifacts: []*a2a.Artifact{
		{Parts: a2a.ContentParts{a2a.NewTextPart("a"), a2a.NewDataPart(map[string]any{"k": 1})}},
		{Parts: a2a.ContentParts{a2a.NewTextPart("b")}},
	}}
	if got := extractReplyText(multi); got != "a\nb" {
		t.Fatalf("extractReplyText = %q, want a\\nb", got)
	}
}

// TestPersistInboxFileMode — the snapshot carries inter-agent message
// text, so it must not be world-readable.
func TestPersistInboxFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not applicable on Windows")
	}
	s := NewStore()
	defer s.Close()
	s.InboxPath = filepath.Join(t.TempDir(), "inbox.json")

	s.deliverIncoming(incoming("m1", "t1", "private"))
	info, err := os.Stat(s.InboxPath)
	if err != nil {
		t.Fatalf("inbox snapshot missing: %v", err)
	}
	if got := info.Mode().Perm(); got != inboxFileMode {
		t.Errorf("inbox file mode = %o, want %o", got, inboxFileMode)
	}
}

// TestSnapshotShapeForHook — the hook reads a JSON array of objects with
// messageId/taskId/contextId/from/text and optional ts, nothing else.
func TestSnapshotShapeForHook(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.InboxPath = filepath.Join(t.TempDir(), "inbox.json")
	s.deliverIncoming(incoming("m1", "t1", "hi"))
	s.TrackOutgoing("out-1", "http://peer/", "peer-B", "q")
	s.IngestOutgoingTerminal(completedTask("out-1", "answer"))

	b, err := os.ReadFile(s.InboxPath)
	if err != nil {
		t.Fatal(err)
	}
	var snap []map[string]any
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatalf("snapshot is not a JSON array of objects: %v", err)
	}
	if len(snap) != 2 {
		t.Fatalf("snapshot entries = %d, want 2", len(snap))
	}
	wantKeys := [][]string{
		{"contextId", "from", "messageId", "taskId", "text"},
		{"contextId", "from", "messageId", "taskId", "text", "ts"},
	}
	for i, entry := range snap {
		keys := make([]string, 0, len(entry))
		for k := range entry {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if fmt.Sprint(keys) != fmt.Sprint(wantKeys[i]) {
			t.Errorf("entry %d keys = %v, want %v", i, keys, wantKeys[i])
		}
	}
}

// TestTrimToRuneSafe — truncation must never slice a multi-byte rune.
func TestTrimToRuneSafe(t *testing.T) {
	in := "вопрос про кириллицу и юникод"
	got := trimTo(in, 10)
	if want := string([]rune(in)[:10]) + "..."; got != want {
		t.Errorf("trimTo = %q, want %q", got, want)
	}
	if short := trimTo("short", 10); short != "short" {
		t.Errorf("trimTo(short) = %q", short)
	}
}

// TestDeliverIncomingDedupByMessageID verifies effectively-once delivery: a
// redelivered message (same MessageID) is not queued twice and its
// side-effects don't fire again.
func TestDeliverIncomingDedupByMessageID(t *testing.T) {
	s := NewStore()
	defer s.Close()
	var fired atomic.Int32
	s.OnIncoming = func(*InboxEntry) { fired.Add(1) }

	if !s.deliverIncoming(incoming("dup1", "t1", "hi")) {
		t.Fatal("first delivery rejected")
	}
	if s.deliverIncoming(incoming("dup1", "t2", "hi")) {
		t.Fatal("redelivery accepted")
	}
	if got := len(s.PeekInbox()); got != 1 {
		t.Fatalf("inbox size = %d after redelivery, want 1 (dedup)", got)
	}
	waitFor(t, "OnIncoming", func() bool { return fired.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := fired.Load(); got != 1 {
		t.Fatalf("OnIncoming fired %d times, want 1", got)
	}
}

// TestLoadInboxRestoresSnapshot verifies durable delivery: entries persisted
// to the snapshot are restored into a fresh store on startup (survive a bounce),
// and a reload after drain is empty.
func TestLoadInboxRestoresSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")

	s1 := NewStore()
	defer s1.Close()
	s1.InboxPath = path
	s1.deliverIncoming(&InboxEntry{MessageID: "keep1", TaskID: "t1", ContextID: "c1", From: "peer-a", Text: "survive me"})

	// Simulate a bounce: a brand-new store pointed at the same snapshot.
	s2 := NewStore()
	defer s2.Close()
	s2.InboxPath = path
	s2.LoadInbox()

	pending := s2.PeekInbox()
	if len(pending) != 1 {
		t.Fatalf("restored inbox size = %d, want 1", len(pending))
	}
	want := InboxEntry{MessageID: "keep1", TaskID: "t1", ContextID: "c1", From: "peer-a", Text: "survive me"}
	if pending[0] != want {
		t.Errorf("restored entry = %+v, want %+v", pending[0], want)
	}

	// Drain, then a fresh reload of the now-empty snapshot must be a no-op.
	if drained := s2.DrainInbox(); len(drained) != 1 {
		t.Fatalf("drain = %d, want 1", len(drained))
	}
	s3 := NewStore()
	defer s3.Close()
	s3.InboxPath = path
	s3.LoadInbox()
	if got := len(s3.PeekInbox()); got != 0 {
		t.Fatalf("reload after drain = %d, want 0", got)
	}

	// The hook truncates the snapshot after rendering; that is a fresh start too.
	if err := os.WriteFile(path, nil, inboxFileMode); err != nil {
		t.Fatal(err)
	}
	s4 := NewStore()
	defer s4.Close()
	s4.InboxPath = path
	s4.LoadInbox()
	if got := len(s4.PeekInbox()); got != 0 {
		t.Fatalf("reload of truncated snapshot = %d, want 0", got)
	}
}

// TestInboxSoftCap verifies the inbox is bounded: past inboxSoftCap the oldest
// entries are dropped (a stuck/never-drained bridge can't grow without bound).
func TestInboxSoftCap(t *testing.T) {
	s := NewStore()
	defer s.Close()
	total := inboxSoftCap + 25
	for i := range total {
		s.deliverIncoming(incoming(fmt.Sprintf("m%05d", i), fmt.Sprintf("t%05d", i), "x"))
	}
	pending := s.PeekInbox()
	if len(pending) != inboxSoftCap {
		t.Fatalf("inbox size = %d, want soft cap %d", len(pending), inboxSoftCap)
	}
	// Oldest dropped → first survivor is index (total-cap).
	if want := fmt.Sprintf("m%05d", total-inboxSoftCap); pending[0].MessageID != want {
		t.Errorf("oldest survivor = %q, want %q", pending[0].MessageID, want)
	}
}

// TestCompleteTaskClearsOutgoingReplyNotification: a synthetic outgoing-reply
// (its TaskID is an outgoing task with no local execution) is cleared by
// CompleteTask rather than returning ErrTaskNotFound, so it does not keep the
// inbox snapshot non-empty. A second delivery of the same task is a no-op.
func TestCompleteTaskClearsOutgoingReplyNotification(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.InboxPath = filepath.Join(t.TempDir(), "inbox.json")
	e := NewExecutor(s, discardLog())
	// PeekInbox consumes one-shot outgoing-replies (the wake-spam fix), so this
	// test reads s.inbox directly to verify the CompleteTask clear-path without the
	// peek-consume side effect.
	inboxLen := func() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.inbox) }

	s.TrackOutgoing("out-1", "http://peer/", "peer-A", "What is 2+2?")
	if !s.IngestOutgoingTerminal(&a2a.Task{
		ID: "out-1",
		Status: a2a.TaskStatus{
			State:   a2a.TaskStateCompleted,
			Message: a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("4")),
		},
	}) {
		t.Fatal("IngestOutgoingTerminal should deliver the tracked reply")
	}

	if got := inboxLen(); got != 1 {
		t.Fatalf("inbox len after reply = %d, want 1", got)
	}
	before, err := os.ReadFile(s.InboxPath)
	if err != nil {
		t.Fatalf("read inbox file: %v", err)
	}
	if !strings.Contains(string(before), "out-1") {
		t.Fatalf("inbox file should contain the outgoing-reply taskId; got %s", before)
	}

	if err := e.CompleteTask("out-1", ""); err != nil {
		t.Fatalf("CompleteTask(outgoing-reply id) = %v, want nil", err)
	}
	if got := inboxLen(); got != 0 {
		t.Fatalf("inbox len after ack = %d, want 0 (reply not cleared)", got)
	}
	after, err := os.ReadFile(s.InboxPath)
	if err != nil {
		t.Fatalf("read inbox file: %v", err)
	}
	if strings.Contains(string(after), "out-1") {
		t.Fatalf("inbox file still retains the outgoing-reply after ack: %s", after)
	}

	if s.IngestOutgoingTerminal(&a2a.Task{ID: "out-1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}) {
		t.Fatal("second IngestOutgoingTerminal should be a no-op (already delivered)")
	}
	if got := inboxLen(); got != 0 {
		t.Fatalf("inbox len after second ingest = %d, want 0 (reply re-appended)", got)
	}

	if err := e.CompleteTask("never-seen", ""); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CompleteTask(unknown id) = %v, want ErrTaskNotFound", err)
	}

	// A genuine inbound entry restored without a live execution is dropped
	// but still reported: nobody is waiting for the reply.
	s.deliverIncoming(incoming("m-restored", "restored", "old question"))
	if err := e.CompleteTask("restored", "late"); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Fatalf("CompleteTask(restored entry) = %v, want ErrTaskNotFound", err)
	}
	if got := inboxLen(); got != 0 {
		t.Fatalf("inbox len after completing restored entry = %d, want 0", got)
	}
}

// TestIngestOutgoingTerminalSkipsEmptyReply verifies the empty-arrivals fix:
// a peer completing our outbound task with NO reply text must not land an
// empty "[ОТВЕТ …]" record in our inbox.
func TestIngestOutgoingTerminalSkipsEmptyReply(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.TrackOutgoing("task-empty", "http://peer/", "peer-A", "ping?")
	if !s.IngestOutgoingTerminal(&a2a.Task{ID: "task-empty", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}) {
		t.Fatalf("IngestOutgoingTerminal = false, want true (tracked + terminal)")
	}
	if pending := s.PeekInbox(); len(pending) != 0 {
		t.Fatalf("inbox size = %d, want 0 (empty reply must be suppressed)", len(pending))
	}
}

// TestEvictExpiredRepliesReapsDeliveredOutgoingReplies verifies the
// stale-re-render fix: aged one-shot outgoing-reply records are evicted by
// identity, while a fresh reply and genuine incoming messages survive.
func TestEvictExpiredRepliesReapsDeliveredOutgoingReplies(t *testing.T) {
	s := NewStore()
	defer s.Close()
	now := time.Now()
	oldTS := now.Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	freshTS := now.UTC().Format(time.RFC3339)
	s.mu.Lock()
	s.inbox = []InboxEntry{
		{MessageID: "reply-old", TaskID: "old", Kind: KindOutgoingReply, TS: oldTS},
		{MessageID: "reply-fresh", TaskID: "fresh", Kind: KindOutgoingReply, TS: freshTS},
		{MessageID: "incoming-1", TaskID: "inc", TS: oldTS}, // genuine incoming → never evicted
	}
	s.mu.Unlock()

	if got := s.evictExpiredReplies(now); got != 1 {
		t.Fatalf("evictExpiredReplies = %d, want 1", got)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inbox) != 2 {
		t.Fatalf("inbox size after evict = %d, want 2 (aged reply reaped)", len(s.inbox))
	}
	for _, e := range s.inbox {
		if e.MessageID == "reply-old" {
			t.Fatal("reply-old should have been evicted")
		}
	}
}

// TestPeekConsumesOutgoingReply verifies the wake-spam fix: PEEK returns an
// outgoing-reply once then consumes it (so it stops re-surfacing every wake),
// while genuine incoming task messages survive peek (stays non-destructive).
func TestPeekConsumesOutgoingReply(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.InboxPath = filepath.Join(t.TempDir(), "inbox.json")
	s.mu.Lock()
	s.inbox = []InboxEntry{
		{MessageID: "incoming-1", TaskID: "t1", Text: "hi"},
		{MessageID: "reply-out1", TaskID: "out1", Text: "done", Kind: KindOutgoingReply},
	}
	s.mu.Unlock()

	if first := s.PeekInbox(); len(first) != 2 {
		t.Fatalf("first peek len=%d, want 2 (returns both once)", len(first))
	}
	second := s.PeekInbox()
	if len(second) != 1 || second[0].MessageID != "incoming-1" {
		t.Fatalf("second peek = %d msgs, want only incoming-1 (reply consumed)", len(second))
	}
}

// TestLoadInboxKeepsReplyTimestamp: a one-shot outgoing-reply restored from the
// snapshot must still age out; without its delivery timestamp a bridge restart
// would resurrect it on every wake.
func TestLoadInboxKeepsReplyTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inbox.json")
	delivered := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	s := NewStore()
	s.InboxPath = path
	s.mu.Lock()
	s.appendInboxLocked(&InboxEntry{
		MessageID: "reply-task-1", TaskID: "task-1", From: "peer", Text: "done",
		Kind: KindOutgoingReply, TS: delivered,
	})
	s.appendInboxLocked(&InboxEntry{MessageID: "incoming-1", TaskID: "task-2", Text: "hello"})
	s.persistInboxLocked()
	s.mu.Unlock()
	s.Close()

	restored := NewStore()
	defer restored.Close()
	restored.InboxPath = path
	restored.LoadInbox()
	restored.evictExpiredReplies(time.Now())

	restored.mu.Lock()
	defer restored.mu.Unlock()
	if len(restored.inbox) != 1 || restored.inbox[0].MessageID != "incoming-1" {
		t.Fatalf("inbox after restart and eviction = %+v, want only incoming-1", restored.inbox)
	}
}
