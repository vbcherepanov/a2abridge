package ideconfig

import (
	"fmt"
	"path/filepath"
)

// claudeCodeWriter handles Claude Code's MCP block and UserPromptSubmit hook.
//
// Claude Code reads user-scope mcpServers from ~/.claude.json (verified on a
// live install — ~/.claude/settings.json does NOT pick up mcpServers; it's an
// undocumented key there), while hooks live in ~/.claude/settings.json. The
// writer therefore touches two files, each with its own timestamped backup:
//
//	~/.claude.json           — "mcpServers": { "a2a": { ... } }
//	~/.claude/settings.json  — "hooks": { "UserPromptSubmit": [ ... ] }
//
// Both writes are idempotent: an up-to-date file is never rewritten.
type claudeCodeWriter struct{}

func (claudeCodeWriter) Name() string { return "Claude Code" }

// claudeMCPConfigPath is the file Claude Code actually reads user-scope MCP
// servers from. Empty string only when the home dir cannot be resolved.
func claudeMCPConfigPath() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".claude.json")
}

// claudeSettingsPath hosts hooks (and legacy mcpServers entries written by
// older a2abridge versions, cleaned up on uninstall).
func claudeSettingsPath() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".claude", "settings.json")
}

func (claudeCodeWriter) Detect() string {
	if p := claudeMCPConfigPath(); fileExists(p) {
		return p
	}
	if p := claudeSettingsPath(); fileExists(p) {
		return p
	}
	// A bare ~/.claude directory is enough evidence Claude Code is installed.
	if h, err := homeDir(); err == nil {
		if d := filepath.Join(h, ".claude"); dirExists(d) {
			return d
		}
	}
	// Default write target — installer will create it as a fresh {} object.
	return claudeMCPConfigPath()
}

func (w claudeCodeWriter) Write(spec Spec, dryRun bool) Result {
	res := writeJSONConfig(w.Name(), claudeMCPConfigPath(), dryRun, func(root map[string]any) bool {
		return setMCPServerEntry(root, spec)
	})
	if res.Error != nil || spec.HookCommand == "" {
		return res
	}

	// Second transaction: the UserPromptSubmit hook goes into settings.json.
	hookRes := writeJSONConfig(w.Name(), claudeSettingsPath(), dryRun, func(root map[string]any) bool {
		if !needsHookUpdate(root, spec) {
			return false
		}
		mergeUserPromptSubmitHook(root, spec.HookCommand)
		return true
	})
	if hookRes.Error != nil {
		res.Error = fmt.Errorf("hook merge in %s: %w", hookRes.Path, hookRes.Error)
		return res
	}
	if hookRes.Updated {
		res.Updated = true
		res.Skipped = false
	}
	return res
}

// needsHookUpdate inspects the existing settings.json hook list and reports
// whether our hook command is already present under UserPromptSubmit. Used
// to avoid unnecessary rewrites (idempotency).
func needsHookUpdate(root map[string]any, spec Spec) bool {
	if spec.HookCommand == "" {
		return false
	}
	hooksRoot, _ := root["hooks"].(map[string]any)
	if hooksRoot == nil {
		return true
	}
	matchers, _ := hooksRoot["UserPromptSubmit"].([]any)
	for _, m := range matchers {
		entry, _ := m.(map[string]any)
		hooks, _ := entry["hooks"].([]any)
		for _, h := range hooks {
			cmd, _ := h.(map[string]any)
			if c, _ := cmd["command"].(string); c == spec.HookCommand {
				return false
			}
		}
	}
	return true
}

// mergeUserPromptSubmitHook appends our hook into root.hooks.UserPromptSubmit
// without disturbing any other hooks the user has registered. The Claude
// Code schema is:
//
//	"hooks": {
//	  "UserPromptSubmit": [
//	    { "matcher": "*", "hooks": [ { "type": "command", "command": "..." } ] }
//	  ]
//	}
//
// We always add our hook under matcher "*" so it fires on every user
// prompt — that's the whole point of the inbox-injection flow.
func mergeUserPromptSubmitHook(root map[string]any, command string) {
	hooksRoot := ensureNestedMap(root, "hooks")
	matchers, _ := hooksRoot["UserPromptSubmit"].([]any)
	ourEntry := map[string]any{
		"type":    "command",
		"command": command,
	}

	// Try to extend an existing matcher "*" group rather than create a new one.
	for _, m := range matchers {
		entry, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if matcher, _ := entry["matcher"].(string); matcher != "*" {
			continue
		}
		hooks, _ := entry["hooks"].([]any)
		for _, h := range hooks {
			if existing, _ := h.(map[string]any); existing != nil {
				if c, _ := existing["command"].(string); c == command {
					return // already present
				}
			}
		}
		entry["hooks"] = append(hooks, ourEntry)
		hooksRoot["UserPromptSubmit"] = matchers
		return
	}

	matchers = append(matchers, map[string]any{
		"matcher": "*",
		"hooks":   []any{ourEntry},
	})
	hooksRoot["UserPromptSubmit"] = matchers
}
