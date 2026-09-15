package agent

import (
	"context"
	"iter"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

// utcExecutor normalizes the timestamps of every event an executor yields to
// UTC. a2a.NewStatusUpdateEvent stamps local time.Now(), which serializes with
// a zone offset; A2A expects ISO 8601 with a Z suffix.
type utcExecutor struct {
	next a2asrv.AgentExecutor
}

var _ a2asrv.AgentExecutor = utcExecutor{}

// Execute implements a2asrv.AgentExecutor.
func (u utcExecutor) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return utcEvents(u.next.Execute(ctx, ec))
}

// Cancel implements a2asrv.AgentExecutor.
func (u utcExecutor) Cancel(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return utcEvents(u.next.Cancel(ctx, ec))
}

func utcEvents(seq iter.Seq2[a2a.Event, error]) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		for ev, err := range seq {
			if !yield(eventInUTC(ev), err) {
				return
			}
		}
	}
}

// eventInUTC rewrites the status timestamp of task and status-update events.
// Messages and artifact updates carry no timestamps.
func eventInUTC(ev a2a.Event) a2a.Event {
	switch v := ev.(type) {
	case *a2a.TaskStatusUpdateEvent:
		statusInUTC(&v.Status)
	case *a2a.Task:
		statusInUTC(&v.Status)
	default:
	}
	return ev
}

func statusInUTC(s *a2a.TaskStatus) {
	if s.Timestamp == nil {
		return
	}
	t := s.Timestamp.UTC()
	s.Timestamp = &t
}
