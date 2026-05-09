package agent

import (
	"context"
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
			ID: taskID,
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
	_, err := s.GetTask(context.Background(), a2a.TaskIDParams{ID: "nope"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err != a2a.ErrTaskNotFound {
		t.Errorf("err = %v, want a2a.ErrTaskNotFound", err)
	}
}
