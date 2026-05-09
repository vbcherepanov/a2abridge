package ideconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestStripPipelineOnFixtures runs the JSONC strip pipeline against
// testdata fixtures shaped like real Claude Code / Cursor / Gemini CLI
// settings files (with line comments, block comments, trailing commas
// for the JSONC sample). The pipeline must produce a string that
// encoding/json successfully parses.
func TestStripPipelineOnFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no fixtures present")
	}
	for _, p := range files {
		t.Run(filepath.Base(p), func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			cleaned := stripJSONComments(b)
			cleaned = stripTrailingCommas(cleaned)
			var obj map[string]any
			if err := json.Unmarshal(cleaned, &obj); err != nil {
				t.Errorf("pipeline failed on %s: %v\n  cleaned head: %q",
					p, err, cleaned[:min(len(cleaned), 80)])
			}
		})
	}
}

// TestClaudeWriteDryRunInTempHome puts a synthetic settings.json under a
// tmp $HOME so the writer's full Write() path is exercised without
// touching the developer's real Claude config.
func TestClaudeWriteDryRunInTempHome(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("testdata/claude-style.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpHome, ".claude", "settings.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}

	w := claudeCodeWriter{}
	res := w.Write(DefaultSpec("/tmp/a2abridge-test"), true)
	if res.Error != nil {
		t.Errorf("dry-run failed: %v (path=%s, found=%v)", res.Error, res.Path, res.Found)
	}
	if !res.Updated {
		t.Errorf("expected Updated=true on a config without an a2a entry, got %+v", res)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
