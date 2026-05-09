package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// User-defined hook scripts fired on A2A events. Looking for files at:
//
//	~/.a2abridge/hooks/on-inbound.<ext>
//	~/.a2abridge/hooks/on-outgoing-reply.<ext>
//	~/.a2abridge/hooks/on-error.<ext>
//
// where <ext> is "sh" on POSIX and "ps1"/"cmd"/"bat" on Windows. The hook
// receives the event payload as JSON on stdin AND in the A2A_EVENT env
// var, plus convenience fields:
//
//	A2A_EVENT_NAME = "on-inbound" | "on-outgoing-reply" | "on-error"
//	A2A_EVENT_FROM = peer name (when applicable)
//	A2A_EVENT_TASK = task id (when applicable)
//
// Hooks are best-effort: errors are swallowed to keep the bridge
// uninterruptible. Each hook is bounded by a 5-second timeout so a
// runaway script can't stall the bridge.

// hookExtensions returns the list of executable extensions in priority
// order for the current OS. POSIX prefers .sh; Windows tries .ps1 → .cmd → .bat.
func hookExtensions() []string {
	if runtime.GOOS == "windows" {
		return []string{".ps1", ".cmd", ".bat"}
	}
	return []string{".sh"}
}

// FireHook runs the user hook with the given event name and payload.
// Non-blocking — the actual exec runs in a goroutine. Safe to call from
// hot paths.
func FireHook(eventName string, payload any) {
	go func() {
		runHook(eventName, payload)
	}()
}

func runHook(eventName string, payload any) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	hooksDir := filepath.Join(home, ".a2abridge", "hooks")

	var script string
	for _, ext := range hookExtensions() {
		candidate := filepath.Join(hooksDir, eventName+ext)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			script = candidate
			break
		}
	}
	if script == "" {
		return
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch filepath.Ext(script) {
	case ".sh":
		cmd = exec.CommandContext(ctx, "/bin/sh", script)
	case ".ps1":
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	case ".cmd", ".bat":
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c", script)
	default:
		return
	}

	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = append(os.Environ(),
		"A2A_EVENT_NAME="+eventName,
		"A2A_EVENT="+string(body),
	)
	if from, ok := payloadField(payload, "from"); ok {
		cmd.Env = append(cmd.Env, "A2A_EVENT_FROM="+from)
	}
	if task, ok := payloadField(payload, "taskId"); ok {
		cmd.Env = append(cmd.Env, "A2A_EVENT_TASK="+task)
	}

	// Discard output — the hook is a fire-and-forget side effect. If the
	// user wants to log, they can inside the script (>>~/log).
	_ = cmd.Run()
}

// payloadField extracts a top-level string field from an arbitrary
// payload. Returns (val, true) if found and stringy, otherwise ("", false).
func payloadField(payload any, key string) (string, bool) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}
