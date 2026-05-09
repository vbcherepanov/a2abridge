package ideconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// clineWriter handles Cline's dedicated MCP file (NOT VS Code settings.json,
// which is JSONC and requires a comment-preserving parser):
//
//	<vs-code-globalStorage>/saoudrizwan.claude-dev/settings/cline_mcp_settings.json
//
// macOS:    ~/Library/Application Support/Code/User/globalStorage/...
// Linux:    ~/.config/Code/User/globalStorage/...
// Windows:  %APPDATA%/Code/User/globalStorage/...
//
// The file format is plain JSON identical to Claude Code's mcpServers block.
type clineWriter struct{}

func (clineWriter) Name() string { return "Cline (VS Code)" }

func (clineWriter) Detect() string {
	root := vsCodeGlobalStorage()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")
}

func (w clineWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.Detect()
	if path == "" {
		res.Error = fmt.Errorf("VS Code globalStorage not found")
		return res
	}
	res.Path = path
	res.Found = fileExists(path)

	// Cline only makes sense if the user has VS Code installed at all. If
	// the parent globalStorage directory is missing, treat as not-installed
	// and exit cleanly without creating a stale tree.
	if !res.Found && !vsCodeInstalled() {
		res.Error = fmt.Errorf("VS Code not detected — skipping")
		return res
	}

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

// vsCodeGlobalStorage returns the path to <vs-code>/User/globalStorage on
// the current OS, or "" if the layout cannot be derived.
func vsCodeGlobalStorage() string {
	switch runtime.GOOS {
	case "darwin":
		if h, err := homeDir(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "Code", "User", "globalStorage")
		}
	case "linux":
		if h, err := homeDir(); err == nil {
			return filepath.Join(h, ".config", "Code", "User", "globalStorage")
		}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Code", "User", "globalStorage")
		}
	}
	return ""
}

func vsCodeInstalled() bool {
	root := vsCodeGlobalStorage()
	if root == "" {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}
