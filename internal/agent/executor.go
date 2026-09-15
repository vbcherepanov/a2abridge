package agent

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/vbcherepanov/a2abridge/v4/internal/metrics"
)

// replyArtifactName names the artifact carrying the host's reply.
const replyArtifactName = "reply"

var (
	// ErrReplyPending is returned by CompleteTask when a reply for the task
	// was already handed over and the execution has not consumed it yet.
	ErrReplyPending = errors.New("a reply for this task is already pending")

	// errWaiterExists guards against two executions waiting on one task.
	// a2a-go rejects concurrent executions, so hitting it means a bug.
	errWaiterExists = errors.New("an execution is already waiting for this task")
)

// Executor is the a2a-go AgentExecutor of a bridge. Every inbound message
// becomes an inbox entry; the execution then stays WORKING until the host
// answers through CompleteTask or the task is canceled.
type Executor struct {
	store *Store
	log   *slog.Logger

	mu      sync.Mutex
	waiters map[a2a.TaskID]chan string
}

var _ a2asrv.AgentExecutor = (*Executor)(nil)

// NewExecutor returns an executor delivering inbound messages into store.
func NewExecutor(store *Store, log *slog.Logger) *Executor {
	return &Executor{
		store:   store,
		log:     log,
		waiters: map[a2a.TaskID]chan string{},
	}
}

// Execute implements a2asrv.AgentExecutor. a2a-go runs it in a context
// detached from the caller, so it survives client disconnects; the context
// is canceled once a cancelation has been applied to the task.
func (e *Executor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if ec.Message == nil {
			yield(nil, fmt.Errorf("execute task %s: message is required: %w", ec.TaskID, a2a.ErrInvalidParams))
			return
		}
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}

		replies, err := e.registerWaiter(ec.TaskID)
		if err != nil {
			yield(nil, fmt.Errorf("execute task %s: %w", ec.TaskID, err))
			return
		}
		defer e.unregisterWaiter(ec.TaskID)

		entry := InboxEntry{
			MessageID: ec.Message.ID,
			TaskID:    string(ec.TaskID),
			ContextID: ec.ContextID,
			From:      metadataString(ec.Message.Metadata, "from"),
			Text:      messageText(ec.Message),
		}
		if !e.store.deliverIncoming(&entry) {
			// The same messageId is already queued under another task: the
			// host answers that one, so this redelivery gets nobody to reply.
			e.log.Info("duplicate inbound message rejected", "task", ec.TaskID, "message", ec.Message.ID)
			notice := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec,
				a2a.NewTextPart(fmt.Sprintf("message %s is already queued for this agent", ec.Message.ID)))
			if yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateRejected, notice), nil) {
				metrics.IncTaskFailed()
			}
			return
		}

		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}

		select {
		case reply := <-replies:
			artifact := a2a.NewArtifactEvent(ec, a2a.NewTextPart(reply))
			artifact.Artifact.Name = replyArtifactName
			artifact.LastChunk = true
			if !yield(artifact, nil) {
				e.log.Warn("reply artifact not recorded", "task", ec.TaskID)
				return
			}
			answer := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(reply))
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, answer), nil) {
				e.log.Warn("task completion not recorded", "task", ec.TaskID)
				return
			}
			metrics.IncTaskCompleted()
		case <-ctx.Done():
		}
	}
}

// Cancel implements a2asrv.AgentExecutor.
func (e *Executor) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil) {
			metrics.IncTaskFailed()
		}
	}
}

// CompleteTask hands the host's reply to the execution waiting on taskID.
// Inbox entries for the task are dropped first, so acknowledging a synthetic
// outgoing-reply (whose task lives on the peer) succeeds without a waiter.
func (e *Executor) CompleteTask(taskID, text string) error {
	droppedReply := e.store.dropTaskEntries(taskID)

	e.mu.Lock()
	defer e.mu.Unlock()
	replies, ok := e.waiters[a2a.TaskID(taskID)]
	if !ok {
		if droppedReply {
			return nil
		}
		return fmt.Errorf("complete task %s: %w", taskID, a2a.ErrTaskNotFound)
	}
	select {
	case replies <- text:
		return nil
	default:
		return fmt.Errorf("complete task %s: %w", taskID, ErrReplyPending)
	}
}

// pendingWaiters reports how many executions are waiting for a reply.
func (e *Executor) pendingWaiters() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.waiters)
}

func (e *Executor) registerWaiter(id a2a.TaskID) (chan string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.waiters[id]; ok {
		return nil, errWaiterExists
	}
	ch := make(chan string, 1)
	e.waiters[id] = ch
	return ch, nil
}

func (e *Executor) unregisterWaiter(id a2a.TaskID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.waiters, id)
}

// metadataString returns meta[key] when it holds a string.
func metadataString(meta map[string]any, key string) string {
	if v, ok := meta[key].(string); ok {
		return v
	}
	return ""
}
