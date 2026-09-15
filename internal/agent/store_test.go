package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/a2a"
)

// TestSendMessageCreatesTaskAndInboxEntry verifies the core round-trip:
// a peer's SendMessage produces a SUBMITTED task in the store and pushes
// the message into our inbox so MCP tools (and the UserPromptSubmit hook)
// can drain it.
func TestSendMessageCreatesTaskAndInboxEntry(t *testing.T) {
	s := NewStore()
	var fired int32
	s.OnIncoming = func(_ a2a.Message) { atomic.AddInt32(&fired, 1) }

	task, msg, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{
			MessageID: "m1",
			Role:      a2a.RoleUser,
			Parts:     []a2a.Part{{Text: "hello"}},
		},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if msg != nil {
		t.Fatal("expected task, got Message variant")
	}
	if task == nil || task.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("task state = %v, want SUBMITTED", task)
	}

	// inbox must contain exactly the message we sent
	pending := s.PeekInbox()
	if len(pending) != 1 {
		t.Fatalf("inbox size = %d, want 1", len(pending))
	}
	if pending[0].MessageID != "m1" {
		t.Errorf("inbox messageId = %q, want m1", pending[0].MessageID)
	}

	// OnIncoming fires asynchronously; poll briefly rather than sleep.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fired) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&fired); got != 1 {
		t.Errorf("OnIncoming fired %d times, want 1", got)
	}

	// GetTask should return the same task we just created
	got, err := s.GetTask(context.Background(), a2a.TaskIDParams{ID: task.ID})
	if err != nil {
		t.Fatalf("getTask: %v", err)
	}
	if got.ID != task.ID {
		t.Errorf("GetTask id = %q, want %q", got.ID, task.ID)
	}
}

// TestPollOutgoingInjectsReply verifies the asymmetric reply-injection
// path: when an outbound task we tracked completes on the peer's side,
// PollOutgoing must drop a synthetic message into our inbox so the hook
// surfaces it on the next user prompt.
func TestPollOutgoingInjectsReply(t *testing.T) {
	s := NewStore()
	s.TrackOutgoing("task-1", "http://peer/", "peer-A", "What is 2+2?")

	fetcher := func(peerURL, taskID string) (*a2a.Task, error) {
		return &a2a.Task{
			ID:     taskID,
			Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
			Artifacts: []a2a.Artifact{
				{Parts: []a2a.Part{{Text: "4"}}},
			},
		}, nil
	}

	completed := s.PollOutgoing(fetcher, 10*time.Minute)
	if completed != 1 {
		t.Fatalf("PollOutgoing returned %d, want 1", completed)
	}

	pending := s.PeekInbox()
	if len(pending) != 1 {
		t.Fatalf("inbox size = %d, want 1", len(pending))
	}
	if pending[0].TaskID != "task-1" {
		t.Errorf("synthetic taskID = %q, want task-1", pending[0].TaskID)
	}
	if len(pending[0].Parts) == 0 || pending[0].Parts[0].Text == "" {
		t.Errorf("synthetic message has no text part: %+v", pending[0])
	}
}

// TestPollOutgoingDropsStaleTask ensures we don't grow pendingOutgoing
// indefinitely when peers never respond.
func TestPollOutgoingDropsStaleTask(t *testing.T) {
	s := NewStore()
	s.TrackOutgoing("stale", "http://peer/", "peer", "?")
	// reach inside to backdate SentAt — that's the simplest way to test
	// the maxAge path without sleeping for real.
	s.mu.Lock()
	s.pendingOutgoing["stale"].SentAt = time.Now().Add(-1 * time.Hour)
	s.mu.Unlock()

	fetcher := func(_, _ string) (*a2a.Task, error) {
		t.Fatal("fetcher should not be called for stale tasks")
		return nil, nil
	}
	_ = s.PollOutgoing(fetcher, 30*time.Minute)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pendingOutgoing["stale"]; ok {
		t.Errorf("stale task still in pendingOutgoing")
	}
}

// TestIngestOutgoingTerminalSSEFastPath verifies that a Task with a
// terminal state delivered via the SSE fast-path produces the same inbox
// entry as the polling path — and removes the task from pendingOutgoing.
func TestIngestOutgoingTerminalSSEFastPath(t *testing.T) {
	s := NewStore()
	s.TrackOutgoing("task-sse", "http://peer/", "peer-A", "ping")

	delivered := s.IngestOutgoingTerminal(&a2a.Task{
		ID:     "task-sse",
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []a2a.Artifact{
			{Parts: []a2a.Part{{Text: "pong"}}},
		},
	})
	if !delivered {
		t.Fatal("IngestOutgoingTerminal should report delivered=true")
	}

	pending := s.PeekInbox()
	if len(pending) != 1 || pending[0].TaskID != "task-sse" {
		t.Fatalf("inbox = %+v, want one entry for task-sse", pending)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pendingOutgoing["task-sse"]; ok {
		t.Errorf("pendingOutgoing still contains task-sse after SSE delivery")
	}
}

// TestIngestOutgoingTerminalIgnoresUntracked drops Tasks that were never
// TrackOutgoing'd — otherwise stray peer notifications could grow the
// inbox with junk.
func TestIngestOutgoingTerminalIgnoresUntracked(t *testing.T) {
	s := NewStore()
	if got := s.IngestOutgoingTerminal(&a2a.Task{
		ID:     "ghost",
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted},
	}); got {
		t.Error("delivered=true for an untracked task")
	}
	if len(s.PeekInbox()) != 0 {
		t.Error("inbox grew despite untracked task")
	}
}

// TestGetTaskNotFoundReturnsSentinel — handlers translate this sentinel
// to JSON-RPC code -32001 (TaskNotFound). Worth a single guard test.
func TestGetTaskNotFoundReturnsSentinel(t *testing.T) {
	s := NewStore()
	defer s.Close()
	_, err := s.GetTask(context.Background(), a2a.TaskIDParams{ID: "nope"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("err = %v, want a2a.ErrTaskNotFound", err)
	}
}

// TestPollOutgoingSkipsAlreadyDelivered reproduces the SSE-vs-poll race:
// the SSE fast path delivers the reply while PollOutgoing is mid network
// fetch. The poll path must re-check pendingOutgoing membership under the
// lock and skip — otherwise the inbox gets the same reply twice.
func TestPollOutgoingSkipsAlreadyDelivered(t *testing.T) {
	s := NewStore()
	defer s.Close()
	s.TrackOutgoing("task-race", "http://peer/", "peer-A", "ping")

	terminal := &a2a.Task{
		ID:        "task-race",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []a2a.Artifact{{Parts: []a2a.Part{{Text: "pong"}}}},
	}
	fetcher := func(_, _ string) (*a2a.Task, error) {
		// Simulate the SSE fast path winning while the poll loop is
		// inside its network fetch (the lock is not held here).
		if !s.IngestOutgoingTerminal(terminal) {
			t.Fatal("SSE fast path should have delivered")
		}
		return terminal, nil
	}

	if got := s.PollOutgoing(fetcher, 10*time.Minute); got != 0 {
		t.Errorf("PollOutgoing completed = %d, want 0 (SSE already delivered)", got)
	}
	if pending := s.PeekInbox(); len(pending) != 1 {
		t.Fatalf("inbox size = %d, want 1 (no double delivery)", len(pending))
	}
}

// TestEvictTerminalTasks covers the janitor: terminal tasks past the TTL
// are evicted together with their push configs; fresh terminal tasks and
// non-terminal tasks survive.
func TestEvictTerminalTasks(t *testing.T) {
	s := NewStore()
	defer s.Close()

	// old terminal task
	doneTask, _, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "q"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTask(doneTask.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePushConfig(context.Background(), a2a.TaskPushNotificationConfig{
		TaskID: doneTask.ID,
		Config: a2a.PushNotificationConfig{URL: "http://wh"},
	}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.tasks[doneTask.ID].Status.Timestamp = time.Now().Add(-2 * terminalTaskTTL)
	s.mu.Unlock()

	// live (non-terminal) task — must never be evicted regardless of age
	liveTask, _, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "q2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.tasks[liveTask.ID].Status.Timestamp = time.Now().Add(-2 * terminalTaskTTL)
	s.mu.Unlock()

	if got := s.evictTerminal(time.Now()); got != 1 {
		t.Fatalf("evictTerminal = %d, want 1", got)
	}
	if _, err := s.GetTask(context.Background(), a2a.TaskIDParams{ID: doneTask.ID}); !errors.Is(err, a2a.ErrTaskNotFound) {
		t.Errorf("evicted task GetTask err = %v, want ErrTaskNotFound", err)
	}
	if _, err := s.GetTask(context.Background(), a2a.TaskIDParams{ID: liveTask.ID}); err != nil {
		t.Errorf("live task evicted: %v", err)
	}
	if cfgs, _ := s.ListPushConfigs(context.Background(), doneTask.ID); len(cfgs) != 0 {
		t.Errorf("push configs survived eviction: %d", len(cfgs))
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

	if _, _, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "private"}}},
	}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.InboxPath)
	if err != nil {
		t.Fatalf("inbox snapshot missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("inbox file mode = %o, want 600", got)
	}
}

// TestSendMessageRejectsTerminalTask — a completed task accepts no more
// input; appending would re-notify subscribers with a stale final status.
func TestSendMessageRejectsTerminalTask(t *testing.T) {
	s := NewStore()
	defer s.Close()
	task, _, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "q"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTask(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{TaskID: task.ID, Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "more"}}},
	}); err == nil {
		t.Fatal("SendMessage into terminal task should error")
	}
}

// TestNotifyFinalDeliveredUnderBackpressure — when a subscriber's buffer
// is full, intermediate events may drop, but the terminal (Final) event
// must still arrive and the channel must be closed afterwards.
func TestNotifyFinalDeliveredUnderBackpressure(t *testing.T) {
	s := NewStore()
	defer s.Close()

	ch := make(chan a2a.StreamResponse, 8)
	s.mu.Lock()
	s.subscribers["t"] = append(s.subscribers["t"], ch)
	// Fill the buffer with 8 non-final events, then push the final one.
	for range 8 {
		s.notifyLocked("t", a2a.StreamResponse{
			StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: "t", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}},
		})
	}
	s.notifyLocked("t", a2a.StreamResponse{
		StatusUpdate: &a2a.TaskStatusUpdateEvent{TaskID: "t", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}, Final: true},
	})
	s.mu.Unlock()

	sawFinal := false
	for ev := range ch { // terminates only if notifyLocked closed the channel
		if ev.StatusUpdate != nil && ev.StatusUpdate.Final {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Error("final event was dropped under backpressure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.subscribers["t"]) != 0 {
		t.Error("subscriber bookkeeping not cleaned up after final event")
	}
}

// TestSubscribeStopsWhenConsumerGone — a consumer that stops reading must
// not leak the Subscribe goroutine: every send is ctx-guarded.
func TestSubscribeStopsWhenConsumerGone(t *testing.T) {
	s := NewStore()
	defer s.Close()
	task, _, err := s.SendMessage(context.Background(), a2a.MessageSendParams{
		Message: a2a.Message{Role: a2a.RoleUser, Parts: []a2a.Part{{Text: "q"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan a2a.StreamResponse) // unbuffered, never read
	done := make(chan error, 1)
	go func() { done <- s.Subscribe(ctx, task.ID, out) }()

	cancel() // consumer is gone
	select {
	case err := <-done:
		if err == nil {
			t.Error("Subscribe returned nil, want ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe goroutine leaked: blocked on send to dead consumer")
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
