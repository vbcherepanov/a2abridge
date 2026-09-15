package agent

import (
	"sync/atomic"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

type recordingCompleter struct{ calls atomic.Int32 }

func (r *recordingCompleter) CompleteTask(string, string) error {
	r.calls.Add(1)
	return nil
}

// TestIsSyntheticReply guards the responder against echo loops: synthetic
// outgoing-reply entries must be skipped, otherwise the responder spawns a
// paid headless LLM run that answers its own peer's answer and then fails
// CompleteTask.
func TestIsSyntheticReply(t *testing.T) {
	cases := []struct {
		name  string
		entry InboxEntry
		want  bool
	}{
		{name: "outgoing-reply kind", entry: InboxEntry{MessageID: "m1", Kind: KindOutgoingReply}, want: true},
		{name: "reply- prefix restored from snapshot", entry: InboxEntry{MessageID: "reply-t1"}, want: true},
		{name: "regular inbound from peer", entry: InboxEntry{MessageID: "m2", From: "peer-A"}, want: false},
		{name: "empty entry", entry: InboxEntry{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.entry.isSyntheticReply(); got != tc.want {
				t.Errorf("isSyntheticReply = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResponderHandleSkipsSyntheticReply — Handle must return before
// spawning anything for synthetic replies, and never complete a task.
func TestResponderHandleSkipsSyntheticReply(t *testing.T) {
	completer := &recordingCompleter{}
	r := &Responder{
		Mode:      "claude",
		Log:       discardLog(),
		Card:      &a2a.AgentCard{Name: "self"},
		Completer: completer,
	}
	r.Handle(&InboxEntry{
		MessageID: "reply-x",
		TaskID:    "x",
		From:      "peer",
		Text:      "peer answer",
		Kind:      KindOutgoingReply,
	})
	if got := completer.calls.Load(); got != 0 {
		t.Errorf("CompleteTask called %d times for a synthetic reply", got)
	}
}

// TestResponderHandleSkipsDisabledAndEmpty — no mode or no text means nothing to do.
func TestResponderHandleSkipsDisabledAndEmpty(t *testing.T) {
	completer := &recordingCompleter{}
	disabled := &Responder{Log: discardLog(), Card: &a2a.AgentCard{}, Completer: completer}
	disabled.Handle(&InboxEntry{MessageID: "m1", TaskID: "t1", Text: "question"})
	empty := &Responder{Mode: "claude", Log: discardLog(), Card: &a2a.AgentCard{}, Completer: completer}
	empty.Handle(&InboxEntry{MessageID: "m2", TaskID: "t2"})
	if got := completer.calls.Load(); got != 0 {
		t.Errorf("CompleteTask called %d times", got)
	}
}
