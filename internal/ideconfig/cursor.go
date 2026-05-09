package ideconfig

import (
	"fmt"
	"path/filepath"
)

// cursorWriter handles Cursor's MCP config at ~/.cursor/mcp.json.
//
// Schema is identical to Claude Code's user-level mcpServers block.
type cursorWriter struct{}

func (cursorWriter) Name() string { return "Cursor" }

func (cursorWriter) Detect() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".cursor", "mcp.json")
}

func (w cursorWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.Detect()
	if path == "" {
		res.Error = fmt.Errorf("could not resolve Cursor config path")
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
	if equalJSON(servers[spec.Key], desired) {
		res.Skipped = true
		return res
	}
	servers[spec.Key] = desired

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
