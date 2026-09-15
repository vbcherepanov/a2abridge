package agent

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// TaskCompleter answers a local task with the host's reply.
type TaskCompleter interface {
	CompleteTask(taskID, text string) error
}

// Responder spawns a headless CLI (`claude -p` or `codex exec`) to auto-answer
// incoming A2A messages, without disturbing the user's interactive session.
type Responder struct {
	Mode      string // "claude" | "codex" | ""
	Log       *slog.Logger
	Card      *a2a.AgentCard
	Completer TaskCompleter
	Timeout   time.Duration

	emptyMCP  string // path to a minimal MCP config so spawned CLI has no tools
	codexHome string // tempdir used as CODEX_HOME for spawned codex (clean config)
}

// NewResponder prepares an empty MCP config file and a fresh CODEX_HOME
// so the spawned CLI is fully isolated (no a2a MCP → no ghost agents).
func NewResponder(mode string, card *a2a.AgentCard, completer TaskCompleter, log *slog.Logger) (*Responder, error) {
	r := &Responder{Mode: mode, Card: card, Completer: completer, Log: log, Timeout: 90 * time.Second}

	// empty MCP config for claude -p --strict-mcp-config
	f, err := os.CreateTemp("", "a2a-empty-mcp-*.json")
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(`{"mcpServers":{}}`); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("write empty MCP config: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("close empty MCP config: %w", err)
	}
	r.emptyMCP = f.Name()

	// CODEX_HOME: скопируем auth.json из оригинала (чтобы авторизация работала),
	// но config.toml напишем минимальный без mcp_servers и AGENTS.md пустой
	// чтобы headless codex не подхватил правила про a2a и не создал шумную активность.
	home, err := os.MkdirTemp("", "a2a-codex-home-*")
	if err != nil {
		_ = os.Remove(r.emptyMCP)
		return nil, err
	}
	origHome := filepath.Join(os.Getenv("HOME"), ".codex")
	if b, rerr := os.ReadFile(filepath.Join(origHome, "auth.json")); rerr == nil {
		if werr := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); werr != nil {
			log.Warn("responder: auth.json copy failed, spawned codex may be unauthenticated", "err", werr)
		}
	} else if !os.IsNotExist(rerr) {
		log.Warn("responder: auth.json read failed, spawned codex may be unauthenticated", "err", rerr)
	}
	if werr := os.WriteFile(filepath.Join(home, "config.toml"), []byte(`
model_reasoning_effort = "low"
approval_policy = "never"
sandbox_mode = "read-only"
`), 0o600); werr != nil {
		log.Warn("responder: config.toml write failed", "err", werr)
	}
	if werr := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("# headless responder — no special rules\n"), 0o600); werr != nil {
		log.Warn("responder: AGENTS.md write failed", "err", werr)
	}
	r.codexHome = home
	return r, nil
}

// Close removes the temp files.
func (r *Responder) Close() {
	if r.emptyMCP != "" {
		_ = os.Remove(r.emptyMCP)
	}
	if r.codexHome != "" {
		_ = os.RemoveAll(r.codexHome)
	}
}

// Handle processes a single inbox entry.
// Intended to be attached to Store.OnIncoming, which runs it in a goroutine.
func (r *Responder) Handle(entry *InboxEntry) {
	if r.Mode == "" || entry == nil {
		return
	}
	// Never answer a synthetic outgoing-reply: spawning a headless LLM run on
	// a peer's answer burns money, produces an echo conversation and then
	// fails CompleteTask because the task lives on the peer's side.
	if entry.isSyntheticReply() {
		return
	}
	question := entry.Text
	if question == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.Timeout)
	defer cancel()

	r.Log.Info("responder spawn", "mode", r.Mode, "task", entry.TaskID, "from", entry.From)

	prompt := r.buildPrompt(entry.From, question)
	reply, err := r.run(ctx, prompt)
	if err != nil {
		r.Log.Warn("responder failed", "err", err, "task", entry.TaskID)
		FireHook("on-error", map[string]any{
			"taskId": entry.TaskID,
			"from":   entry.From,
			"error":  err.Error(),
		})
		return
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		r.Log.Warn("responder empty reply", "task", entry.TaskID)
		return
	}
	if err := r.Completer.CompleteTask(entry.TaskID, reply); err != nil {
		r.Log.Warn("responder complete failed", "err", err, "task", entry.TaskID)
		FireHook("on-error", map[string]any{
			"taskId": entry.TaskID,
			"from":   entry.From,
			"error":  err.Error(),
		})
		return
	}
	r.Log.Info("responder answered", "task", entry.TaskID, "len", len(reply))
}

func (r *Responder) buildPrompt(from, question string) string {
	names := make([]string, 0, len(r.Card.Skills))
	for i := range r.Card.Skills {
		names = append(names, r.Card.Skills[i].ID)
	}
	if len(names) > 10 {
		names = names[:10]
	}
	skills := strings.Join(names, ",")
	if skills == "" {
		skills = "generic"
	}
	return fmt.Sprintf(
		`Ты автономный A2A-агент "%s" (skills: %s). Отвечаешь другому агенту кратко и по делу, без markdown-хедеров. Если вопрос вне твоей компетенции — одной строкой скажи об этом.

Вопрос от агента "%s":
%s

Ответ (одной порцией, без преамбул):`,
		r.Card.Name, skills, from, question,
	)
}

func (r *Responder) run(ctx context.Context, prompt string) (string, error) {
	switch r.Mode {
	case "claude":
		return r.runClaude(ctx, prompt)
	case "codex":
		return r.runCodex(ctx, prompt)
	default:
		return "", fmt.Errorf("unknown responder mode: %s", r.Mode)
	}
}

func (r *Responder) runClaude(ctx context.Context, prompt string) (string, error) {
	claudeBin := firstOnPath("claude")
	if claudeBin == "" {
		return "", fmt.Errorf("claude binary not found")
	}
	// --strict-mcp-config предотвращает merge с ~/.claude.json, иначе spawned
	// claude подхватит "a2a" из user config и зарегистрируется как ещё один агент.
	cmd := exec.CommandContext(ctx, claudeBin,
		"-p",
		"--permission-mode=acceptEdits",
		"--output-format=text",
		"--mcp-config", r.emptyMCP,
		"--strict-mcp-config",
	)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(),
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_DISABLE_SESSION_START=1",
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, errb.String())
	}
	return out.String(), nil
}

func (r *Responder) runCodex(ctx context.Context, prompt string) (string, error) {
	codexBin := firstOnPath("codex")
	if codexBin == "" {
		return "", fmt.Errorf("codex binary not found")
	}
	// "--" terminates flag parsing so a peer-controlled prompt starting
	// with a dash can never be parsed as a codex flag.
	cmd := exec.CommandContext(ctx, codexBin,
		"exec",
		"--skip-git-repo-check",
		"--",
		prompt,
	)
	cmd.Stdin = nil
	// Подменяем CODEX_HOME на временный каталог — без mcp_servers и правил,
	// чтобы headless codex не регистрировал ghost-bridge в directory.
	env := []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CODEX_HOME=") {
			env = append(env, e)
		}
	}
	env = append(env, "CODEX_HOME="+r.codexHome)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex exec: %w: %s", err, errb.String())
	}
	// codex exec печатает много служебных строк + финальный ответ.
	// Берём всё что после последней маркерной строки "codex".
	return extractCodexAnswer(out.String()), nil
}

// extractCodexAnswer пробует вытащить финальный ответ модели из вывода `codex exec`.
// Структура вывода:
//
//	reasoning effort: ...
//	session id: ...
//	--------
//	user
//	<prompt>
//	codex
//	<answer>
//	tokens used
//	...
var codexBlockRe = regexp.MustCompile(`(?ms)^codex\s*\n(.*?)\ntokens used`)

func extractCodexAnswer(s string) string {
	if m := codexBlockRe.FindStringSubmatch(s); len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	// fallback: последняя непустая строка
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" {
			return l
		}
	}
	return ""
}

func firstOnPath(candidates ...string) string {
	for _, c := range candidates {
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
			continue
		}
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}
