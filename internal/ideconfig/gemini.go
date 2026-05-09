package ideconfig

import (
	"fmt"
	"path/filepath"
)

// geminiWriter handles Gemini CLI's settings file at
// ~/.gemini/settings.json. Schema mirrors Claude Code / Cursor — same
// mcpServers block.
type geminiWriter struct{}

func (geminiWriter) Name() string { return "Gemini CLI" }

func (geminiWriter) Detect() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".gemini", "settings.json")
}

func (w geminiWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.Detect()
	if path == "" {
		res.Error = fmt.Errorf("could not resolve Gemini CLI config path")
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
