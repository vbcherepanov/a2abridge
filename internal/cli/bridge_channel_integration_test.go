//go:build integration

package cli_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
)

// TestBridgeChannelWakesSession is the #46 journey against the real binary:
// a bridge started with A2A_NUDGE=channel is driven over stdio the way
// Claude Code 2.1.332 drives it (server/discover probe, then initialize), a
// peer sends it an A2A message, and the message comes out on stdout as a
// notifications/claude/channel event.
func TestBridgeChannelWakesSession(t *testing.T) {
	stateDir := t.TempDir()
	send, next := startStdioBridge(t, stateDir, "channel")
	byID := func(id string) func(map[string]any) bool {
		return func(m map[string]any) bool { return m["id"] == id }
	}

	send(claudeCodeProbe("probe"))
	probe, _ := next("server/discover result", byID("probe"))["result"].(map[string]any)
	if got := fmt.Sprint(probe["supportedVersions"]); strings.Contains(got, "2026-07-28") {
		t.Fatalf("channel-mode bridge advertises %s; Claude Code would skip channel events", got)
	}
	send(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.332"}}}`)
	init, _ := next("initialize result", byID("init"))["result"].(map[string]any)
	if !strings.Contains(fmt.Sprint(init["capabilities"]), "claude/channel") {
		t.Fatalf("initialize capabilities = %v, want experimental claude/channel", init["capabilities"])
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	port := waitForPort(t, stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	card, err := agentcard.DefaultResolver.Resolve(ctx, fmt.Sprintf("http://127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("resolve card: %v", err)
	}
	client, err := a2aclient.NewFromCard(ctx, card)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = client.Destroy() }()
	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("e2e is green, please deploy"))
	msg.Metadata = map[string]any{"from": "hermes"}
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{Message: msg, Config: &a2a.SendMessageConfig{ReturnImmediately: true}})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	task, ok := res.(*a2a.Task)
	if !ok {
		t.Fatalf("send result = %T, want *a2a.Task", res)
	}

	ev := next("channel event", func(m map[string]any) bool { return m["method"] == "notifications/claude/channel" })
	params, _ := ev["params"].(map[string]any)
	meta, _ := params["meta"].(map[string]any)
	if params["content"] != "e2e is green, please deploy" || meta["claimed_from"] != "hermes" || meta["task_id"] != string(task.ID) {
		t.Fatalf("channel event params = %v, want the message, claimed_from hermes and task_id %s", params, task.ID)
	}
}

// startStdioBridge runs the real bridge with A2A_NUDGE=nudge and returns a
// writer for its stdin and a reader that waits for a matching stdout line.
func startStdioBridge(t *testing.T, stateDir, nudge string) (send func(string), next func(string, func(map[string]any) bool) map[string]any) {
	t.Helper()
	cmd := exec.Command(bridgeBin, bridgeArgs("127.0.0.1:0", stateDir, "nudge-"+nudge)...)
	// TMUX set: a2a's auto detection would pick tmux there, never channel.
	cmd.Env = append(os.Environ(), "A2A_NUDGE="+nudge, "A2A_RESPONDER=", "TMUX=/tmp/itest-tmux,1,0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	lines := make(chan map[string]any, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				lines <- m
			}
		}
	}()
	send = func(s string) {
		t.Helper()
		if _, err := io.WriteString(stdin, s+"\n"); err != nil {
			t.Fatalf("write to bridge stdin: %v", err)
		}
	}
	next = func(what string, match func(map[string]any) bool) map[string]any {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case m, ok := <-lines:
				if !ok {
					t.Fatalf("bridge stdout closed before %s", what)
				}
				if match(m) {
					return m
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	return send, next
}

// TestBridgeVersionLimitOnlyInChannelMode pins the other half of #46: the
// legacy-only version list and the claude/channel capability belong to
// A2A_NUDGE=channel alone. Unset and auto keep mcp-go's defaults.
func TestBridgeVersionLimitOnlyInChannelMode(t *testing.T) {
	for _, nudge := range []string{"", "auto"} {
		t.Run("nudge="+nudge, func(t *testing.T) {
			send, next := startStdioBridge(t, t.TempDir(), nudge)
			send(claudeCodeProbe("probe"))
			probe, _ := next("server/discover result", func(m map[string]any) bool { return m["id"] == "probe" })["result"].(map[string]any)
			if got := fmt.Sprint(probe["supportedVersions"]); !strings.Contains(got, "2026-07-28") {
				t.Errorf("A2A_NUDGE=%q offers %s, want mcp-go's default list with 2026-07-28", nudge, got)
			}
			send(`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.332"}}}`)
			init, _ := next("initialize result", func(m map[string]any) bool { return m["id"] == "init" })["result"].(map[string]any)
			if got := fmt.Sprint(init["capabilities"]); strings.Contains(got, "claude/channel") {
				t.Errorf("A2A_NUDGE=%q declares claude/channel: %s", nudge, got)
			}
		})
	}
}

// claudeCodeProbe is the server/discover request Claude Code 2.1.332 sends
// first, copied from a captured session.
func claudeCodeProbe(id string) string {
	return `{"jsonrpc":"2.0","id":"` + id + `","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","version":"2.1.332"},"io.modelcontextprotocol/clientCapabilities":{"roots":{"listChanged":true}}}}}`
}
