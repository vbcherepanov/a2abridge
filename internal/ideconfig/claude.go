package ideconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// claudeCodeWriter handles Claude Code's MCP block.
//
// Claude Code stores user-level MCP servers in one of two files; we look at
// both and prefer the one that already exists. Both have the same schema:
//
//	{
//	  "mcpServers": {
//	    "a2a": { "command": "...", "args": ["bridge"], "env": {...} }
//	  }
//	}
type claudeCodeWriter struct{}

func (claudeCodeWriter) Name() string { return "Claude Code" }

func (claudeCodeWriter) Detect() string {
	for _, p := range claudeCodeCandidatePaths() {
		if fileExists(p) {
			return p
		}
	}
	// Default to ~/.claude/settings.json — installer will create it.
	if h, err := homeDir(); err == nil {
		return filepath.Join(h, ".claude", "settings.json")
	}
	return ""
}

func (w claudeCodeWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.Detect()
	if path == "" {
		res.Error = fmt.Errorf("could not resolve Claude Code config path")
		return res
	}
	res.Path = path
	res.Found = fileExists(path)

	root, err := readJSONObject(path)
	if err != nil {
		res.Error = err
		return res
	}

	servers := ensureNestedMap(root, "mcpServers")
	desired := mcpEntryJSON(spec)
	mcpUpToDate := equalJSON(servers[spec.Key], desired)
	hookUpToDate := !needsHookUpdate(root, spec)
	if mcpUpToDate && hookUpToDate {
		res.Skipped = true
		return res
	}
	servers[spec.Key] = desired
	if spec.HookCommand != "" {
		mergeUserPromptSubmitHook(root, spec.HookCommand)
	}

	if dryRun {
		res.Updated = true
		return res
	}

	if res.Found {
		bak, berr := backupFile(path)
		if berr != nil {
			res.Error = fmt.Errorf("backup: %w", berr)
			return res
		}
		res.Backup = bak
	}
	if err := writeJSONObject(path, root); err != nil {
		res.Error = err
		return res
	}
	res.Updated = true
	return res
}

// claudeCodeCandidatePaths returns the list of locations Claude Code may
// read from, in priority order.
func claudeCodeCandidatePaths() []string {
	h, err := homeDir()
	if err != nil {
		return nil
	}
	paths := []string{
		filepath.Join(h, ".claude", "settings.json"),
		filepath.Join(h, ".claude.json"),
	}
	// Windows often duplicates under %USERPROFILE%, but UserHomeDir already
	// returns it on Windows — keep one code path.
	if runtime.GOOS == "windows" {
		_ = os.Getenv // keep the import live (Windows-only branches expand later)
	}
	return paths
}

// mcpEntryJSON renders the MCP server block in the format every JSON-based
// IDE we support agrees on (Claude Code, Cursor, Cline, Gemini CLI).
func mcpEntryJSON(spec Spec) map[string]any {
	envObj := make(map[string]any, len(spec.Env))
	for k, v := range spec.Env {
		envObj[k] = v
	}
	entry := map[string]any{
		"command": spec.BinaryPath,
		"args":    []any{"bridge"},
	}
	if len(envObj) > 0 {
		entry["env"] = envObj
	}
	return entry
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
