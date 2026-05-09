package ideconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
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

// removeCodexEntry strips [mcp_servers.a2a] from Codex's TOML config.
func removeCodexEntry(path string) error {
	if !fileExists(path) {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	root := map[string]any{}
	if len(b) > 0 {
		if err := toml.Unmarshal(b, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	if _, ok := servers["a2a"]; !ok {
		return nil
	}
	delete(servers, "a2a")
	if len(servers) == 0 {
		delete(root, "mcp_servers")
	}

	if _, err := backupFile(path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
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
