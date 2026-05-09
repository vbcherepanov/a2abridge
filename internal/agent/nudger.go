package agent

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/a2a"
)

// DetectNudgeMode picks the best cross-platform backend available.
// Priority: explicit TMUX env (inside tmux session) > macOS Terminal.app > none.
// Callers can still force a specific mode via env.
func DetectNudgeMode() string {
	if os.Getenv("TMUX") != "" {
		return "tmux"
	}
	if runtime.GOOS == "darwin" {
		return "terminal" // fallback for user-facing macOS terminal
	}
	return ""
}

// Nudger programmatically types text into the parent interactive session's
// terminal tab so that an incoming A2A message creates a new turn in the
// live Claude/Codex process without requiring the user to type.
// Current backend: macOS Terminal.app via osascript. iTerm2 and tmux can be
// added later.
type Nudger struct {
	Mode string // "terminal" | "iterm2" | "tmux" | ""
	TTY  string // e.g. "/dev/ttys003"
	Log  *slog.Logger

	mu          sync.Mutex
	lastNudgeAt time.Time
	coalesceFor time.Duration // skip nudging if we nudged very recently
}

// NewNudger validates inputs and returns a ready nudger.
func NewNudger(mode, tty string, log *slog.Logger) *Nudger {
	if !strings.HasPrefix(tty, "/dev/") {
		tty = "/dev/" + tty
	}
	return &Nudger{Mode: mode, TTY: tty, Log: log, coalesceFor: 3 * time.Second}
}

// Handle is meant to be attached to Store.OnIncoming.
// It types a short directive into the parent terminal which triggers the
// live agent (Claude/Codex) to process its inbox on the very next turn.
func (n *Nudger) Handle(_ a2a.Message) {
	if n.Mode == "" {
		return
	}
	n.mu.Lock()
	if time.Since(n.lastNudgeAt) < n.coalesceFor {
		n.mu.Unlock()
		return
	}
	n.lastNudgeAt = time.Now()
	n.mu.Unlock()

	text := "проверь a2a_inbox и обработай входящие сообщения — ответь через a2a_complete_task тем кому адресовано"
	if err := n.nudge(text); err != nil {
		n.Log.Warn("nudge failed", "err", err, "tty", n.TTY, "mode", n.Mode)
	} else {
		n.Log.Info("nudged", "tty", n.TTY, "mode", n.Mode)
	}
}

func (n *Nudger) nudge(text string) error {
	switch n.Mode {
	case "terminal":
		return n.nudgeTerminal(text)
	case "iterm2":
		return n.nudgeITerm2(text)
	case "tmux":
		return n.nudgeTmux(text)
	default:
		return fmt.Errorf("unknown nudge mode: %s", n.Mode)
	}
}

// nudgeTerminal finds the Terminal.app tab with the target TTY, brings it
// to the foreground just long enough to send keystrokes (text + Return),
// then restores whatever app the user was focused on.
// Uses System Events `keystroke` for BOTH text and Return to guarantee the
// input is delivered as real keyboard events (raw-mode TUIs like codex need
// this — `do script` sends \n via shell stdin which the TUI doesn't treat
// as submit). tmux backend is preferred when available.
func (n *Nudger) nudgeTerminal(text string) error {
	esc := strings.ReplaceAll(text, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)

	script := fmt.Sprintf(`set targetWin to missing value
set targetTab to missing value
tell application "Terminal"
  repeat with w in every window
    repeat with t in every tab of w
      try
        if tty of t is "%s" then
          set targetWin to w
          set targetTab to t
          exit repeat
        end if
      end try
    end repeat
    if targetTab is not missing value then exit repeat
  end repeat
  if targetTab is missing value then return "no-tab"
end tell

-- remember the user's current frontmost app so we can restore
tell application "System Events"
  set prevFront to name of first process whose frontmost is true
end tell

-- bring exactly the target window+tab to front
tell application "Terminal"
  activate
  set frontmost of targetWin to true
  set index of targetWin to 1
  set selected tab of targetWin to targetTab
end tell
delay 0.15

-- type text + Return as real keystrokes
tell application "System Events"
  keystroke "%s"
  delay 0.05
  key code 36
end tell

-- restore previous frontmost
delay 0.05
try
  tell application "System Events"
    set frontmost of (first process whose name is prevFront) to true
  end tell
end try
return "ok"`, n.TTY, esc)

	cmd := exec.Command("osascript", "-e", script)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("osascript: %w: %s", err, errb.String())
	}
	if strings.TrimSpace(out.String()) != "ok" {
		return fmt.Errorf("target tab not found for tty=%s (output=%q)", n.TTY, out.String())
	}
	return nil
}

// nudgeITerm2 — TODO (iTerm2 has a richer Python/AppleScript API).
func (n *Nudger) nudgeITerm2(text string) error {
	return fmt.Errorf("iterm2 backend not implemented yet")
}

// nudgeTmux: tmux send-keys -t <target> "<text>" Enter.
// Target is derived from TTY by walking tmux sessions.
func (n *Nudger) nudgeTmux(text string) error {
	out, err := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_tty} #{session_name}:#{window_index}.#{pane_index}").Output()
	if err != nil {
		return fmt.Errorf("tmux list-panes: %w", err)
	}
	var target string
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if parts[0] == n.TTY {
			target = parts[1]
			break
		}
	}
	if target == "" {
		return fmt.Errorf("no tmux pane with tty=%s", n.TTY)
	}
	cmd := exec.Command("tmux", "send-keys", "-t", target, text, "Enter")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux send-keys: %w: %s", err, errb.String())
	}
	return nil
}
