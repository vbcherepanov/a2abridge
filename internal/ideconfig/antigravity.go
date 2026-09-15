package ideconfig

import (
	"os"
	"path/filepath"
)

// antigravityWriter handles Antigravity CLI (agy). It reads MCP servers from
// ~/.gemini/config/mcp_config.json, not from Gemini CLI's
// ~/.gemini/settings.json; the mcpServers shape matches Claude Code.
type antigravityWriter struct{}

func (antigravityWriter) Name() string { return "Antigravity CLI" }

// Detect answers "is Antigravity CLI installed?". mcp_config.json may not
// exist before the first MCP server is added, so the CLI's own state
// directory ~/.gemini/antigravity-cli also counts as a marker.
func (w antigravityWriter) Detect() string {
	if target := w.writeTarget(); fileExists(target) {
		return target
	}
	h, err := homeDir()
	if err != nil {
		return ""
	}
	marker := filepath.Join(h, ".gemini", "antigravity-cli")
	if info, err := os.Stat(marker); err == nil && info.IsDir() {
		return marker
	}
	return ""
}

// writeTarget is the global MCP config Antigravity CLI reads.
func (antigravityWriter) writeTarget() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".gemini", "config", "mcp_config.json")
}

func (w antigravityWriter) Write(spec Spec, dryRun bool) Result {
	return writeJSONConfig(w.Name(), w.writeTarget(), dryRun, func(root map[string]any) bool {
		return setMCPServerEntry(root, spec)
	})
}
