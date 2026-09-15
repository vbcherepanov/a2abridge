package agent

import (
	"log/slog"
	"testing"
)

// TestNewDtachNudgerConfig verifies the dtach backend constructor wires Mode +
// Socket (the opt-in native-hook wake path for dtach-supervised bots).
func TestNewDtachNudgerConfig(t *testing.T) {
	n := NewDtachNudger("/tmp/x.sock", slog.Default())
	if n.Mode != "dtach" {
		t.Errorf("Mode = %q, want dtach", n.Mode)
	}
	if n.Socket != "/tmp/x.sock" {
		t.Errorf("Socket = %q, want /tmp/x.sock", n.Socket)
	}
}

// TestNudgeDtachRequiresSocket guards against a misconfigured dtach nudger:
// an empty socket must error (and dispatch must route "dtach" → nudgeDtach),
// not panic or silently no-op.
func TestNudgeDtachRequiresSocket(t *testing.T) {
	n := &Nudger{Mode: "dtach"} // no socket configured
	if err := n.nudge("hi"); err == nil {
		t.Fatal("nudge(dtach) with no socket should error, got nil")
	}
}

// TestNudgeUnknownMode keeps the dispatch default-branch honest.
func TestNudgeUnknownMode(t *testing.T) {
	n := &Nudger{Mode: "bogus"}
	if err := n.nudge("hi"); err == nil {
		t.Fatal("nudge with unknown mode should error")
	}
}
