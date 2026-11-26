package ideconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// removeJSONMCPEntry removes mcpServers.a2a from a JSON config. It also
// strips our UserPromptSubmit hook entry from settings.json — without
// disturbing other hooks or other MCP servers the user might have. We
// always back up before write.
func removeJSONMCPEntry(path string) error {
	if !fileExists(path) {
		return nil
	}
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	changed := false
	if servers, _ := root["mcpServers"].(map[string]any); servers != nil {
		if _, ok := servers["a2a"]; ok {
			delete(servers, "a2a")
			changed = true
		}
		if len(servers) == 0 {
			delete(root, "mcpServers")
		}
	}
	if hooks, _ := root["hooks"].(map[string]any); hooks != nil {
		if pruneUserPromptSubmitHook(hooks) {
			changed = true
		}
		if len(hooks) == 0 {
			delete(root, "hooks")
		}
	}
	if !changed {
		return nil
	}
	if _, err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return writeJSONObject(path, root)
}

// pruneUserPromptSubmitHook removes any hook entry whose command path
// ends with "/a2a-inbox-hook.sh". Returns true if anything was removed.
func pruneUserPromptSubmitHook(hooks map[string]any) bool {
	matchers, _ := hooks["UserPromptSubmit"].([]any)
	if matchers == nil {
		return false
	}
	changed := false
	out := matchers[:0]
	for _, m := range matchers {
		entry, _ := m.(map[string]any)
		if entry == nil {
			out = append(out, m)
			continue
		}
		hookList, _ := entry["hooks"].([]any)
		filtered := hookList[:0]
		for _, h := range hookList {
			cmd, _ := h.(map[string]any)
			c, _ := cmd["command"].(string)
			if filepath.Base(c) == "a2a-inbox-hook.sh" {
				changed = true
				continue
			}
			filtered = append(filtered, h)
		}
		if len(filtered) == 0 {
			// drop the whole matcher entry — it has no hooks left
			changed = true
			continue
		}
		entry["hooks"] = filtered
		out = append(out, entry)
	}
	if !changed {
		return false
	}
	if len(out) == 0 {
		delete(hooks, "UserPromptSubmit")
	} else {
		hooks["UserPromptSubmit"] = out
	}
	return true
}

// removeCodexEntry strips [mcp_servers.a2a] and its sub-tables from Codex's
// TOML config, leaving the rest of the file byte for byte as it was.
func removeCodexEntry(path string) error {
	if !fileExists(path) {
		return nil
	}
	raw, root, err := readTOMLConfig(path)
	if err != nil {
		return err
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	if _, ok := servers[codexServerKey]; !ok {
		return nil
	}
	delete(servers, codexServerKey)
	if len(servers) == 0 {
		delete(root, "mcp_servers")
	}

	out, _, err := renderTOMLConfig(root, func() ([]byte, error) {
		return stripCodexEntry(raw, codexServerKey), nil
	})
	if err != nil {
		return err
	}
	if _, err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return writeFileAtomic(path, out, newConfigMode)
}

// removeClaudeEntries cleans both Claude Code files: mcpServers.a2a in
// ~/.claude.json plus the UserPromptSubmit hook and any legacy
// mcpServers.a2a left in ~/.claude/settings.json by older a2abridge
// versions. Each file gets its own backup before modification.
func removeClaudeEntries() error {
	var errs []error
	if p := claudeMCPConfigPath(); p != "" {
		if err := removeJSONMCPEntry(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	if p := claudeSettingsPath(); p != "" {
		if err := removeJSONMCPEntry(p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// removeContinueFile deletes the dedicated a2a.yaml the installer wrote.
// We back up to <name>.bak.<ts> rather than just rm — Continue's flow is
// the only one where the file is wholly ours, so a backup is the safe
// reversible choice.
func removeContinueFile(_ string) error {
	w := continueWriter{}
	target := w.writeTarget()
	if !fileExists(target) {
		return nil
	}
	bak, err := backupFile(target)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	_ = bak
	return nil
}
