package ideconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestContinueDetectionAndUninstall is the regression for the bug where
// Continue was never detected (Detect returns the ~/.continue DIRECTORY,
// and the old WriterFound demanded a regular file), which made auto-install
// skip Continue and left removeContinueFile unreachable on uninstall.
func TestContinueDetectionAndUninstall(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".continue"), 0o755); err != nil {
		t.Fatal(err)
	}

	w := &continueWriter{}
	if !WriterFound(w) {
		t.Fatalf("WriterFound(continue) = false with ~/.continue present; Detect() = %q", w.Detect())
	}

	res := w.Write(DefaultSpec("/tmp/a2abridge-test"), false)
	if res.Error != nil {
		t.Fatalf("Write: %v", res.Error)
	}
	target := filepath.Join(tmpHome, ".continue", "mcpServers", "a2a.yaml")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("a2a.yaml not written: %v", err)
	}

	// Uninstall path: RemoveMCPEntry must reach removeContinueFile.
	if err := RemoveMCPEntry(w, w.Detect()); err != nil {
		t.Fatalf("RemoveMCPEntry: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a2a.yaml still present after uninstall (stat err = %v)", err)
	}
}

// TestWriteJSONObjectPreservesPermissions checks that rewriting an existing
// config keeps its file mode and that brand-new configs default to 0600
// (they may hold tokens).
func TestWriteJSONObjectPreservesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits not applicable on Windows")
	}
	dir := t.TempDir()

	existing := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(existing, []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONObject(existing, map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(existing)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("existing file mode = %o, want 640", got)
	}

	fresh := filepath.Join(dir, "sub", "fresh.json")
	if err := writeJSONObject(fresh, map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("new file mode = %o, want 600", got)
	}
}

// TestReadJSONObjectKeepsBigIntegers ensures the UseNumber decoder keeps
// integers above 2^53 intact through a read → write round-trip (plain
// float64 decoding would corrupt them).
func TestReadJSONObjectKeepsBigIntegers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	const big = "9007199254740993" // 2^53 + 1
	if err := os.WriteFile(path, []byte(`{"projectId": `+big+`}`), 0o644); err != nil {
		t.Fatal(err)
	}

	root, err := readJSONObject(path)
	if err != nil {
		t.Fatal(err)
	}
	num, ok := root["projectId"].(json.Number)
	if !ok {
		t.Fatalf("projectId decoded as %T, want json.Number", root["projectId"])
	}
	if num.String() != big {
		t.Errorf("projectId = %s, want %s", num, big)
	}

	if err := writeJSONObject(path, root); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), big) {
		t.Errorf("round-tripped file lost the big integer:\n%s", out)
	}
}

// TestClaudeWritesMCPToClaudeJSON verifies the Claude Code writer targets
// ~/.claude.json for mcpServers (the file Claude Code actually reads) and
// ~/.claude/settings.json for hooks only.
func TestClaudeWritesMCPToClaudeJSON(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	spec := DefaultSpec("/tmp/a2abridge-test")
	spec.HookCommand = filepath.Join(tmpHome, ".claude", "hooks", "a2a-inbox-hook.sh")

	w := claudeCodeWriter{}
	res := w.Write(spec, false)
	if res.Error != nil {
		t.Fatalf("Write: %v", res.Error)
	}
	if !res.Updated {
		t.Fatalf("expected Updated=true, got %+v", res)
	}

	mcpPath := filepath.Join(tmpHome, ".claude.json")
	mcpRoot, err := readJSONObject(mcpPath)
	if err != nil {
		t.Fatalf("read %s: %v", mcpPath, err)
	}
	servers, _ := mcpRoot["mcpServers"].(map[string]any)
	if servers == nil || servers["a2a"] == nil {
		t.Errorf("mcpServers.a2a missing in ~/.claude.json: %v", mcpRoot)
	}

	settingsPath := filepath.Join(tmpHome, ".claude", "settings.json")
	settingsRoot, err := readJSONObject(settingsPath)
	if err != nil {
		t.Fatalf("read %s: %v", settingsPath, err)
	}
	if _, has := settingsRoot["mcpServers"]; has {
		t.Errorf("mcpServers must NOT be written into settings.json: %v", settingsRoot)
	}
	if _, has := settingsRoot["hooks"]; !has {
		t.Errorf("hooks missing in settings.json: %v", settingsRoot)
	}

	// Idempotency: a second Write must skip both files.
	res2 := w.Write(spec, false)
	if res2.Error != nil || !res2.Skipped {
		t.Errorf("second Write expected Skipped, got %+v", res2)
	}
}

// TestRemoveClaudeEntriesCleansBothFiles checks uninstall removes the a2a
// entry from ~/.claude.json AND any legacy mcpServers.a2a + hook left in
// ~/.claude/settings.json by older versions.
func TestRemoveClaudeEntriesCleansBothFiles(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("USERPROFILE", tmpHome)
	if err := os.MkdirAll(filepath.Join(tmpHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	mcpPath := filepath.Join(tmpHome, ".claude.json")
	legacyPath := filepath.Join(tmpHome, ".claude", "settings.json")
	current := `{"mcpServers":{"a2a":{"command":"/x"},"other":{"command":"/y"}}}`
	legacy := `{
		"mcpServers": {"a2a": {"command": "/old"}},
		"hooks": {"UserPromptSubmit": [
			{"matcher": "*", "hooks": [{"type": "command", "command": "/home/u/.claude/hooks/a2a-inbox-hook.sh"}]}
		]}
	}`
	if err := os.WriteFile(mcpPath, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	w := &claudeCodeWriter{}
	if err := RemoveMCPEntry(w, w.Detect()); err != nil {
		t.Fatalf("RemoveMCPEntry: %v", err)
	}

	mcpRoot, err := readJSONObject(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := mcpRoot["mcpServers"].(map[string]any)
	if servers == nil {
		t.Fatalf("mcpServers wiped entirely from ~/.claude.json, other servers must survive: %v", mcpRoot)
	}
	if _, has := servers["a2a"]; has {
		t.Errorf("a2a still present in ~/.claude.json")
	}
	if _, has := servers["other"]; !has {
		t.Errorf("unrelated server removed from ~/.claude.json")
	}

	legacyRoot, err := readJSONObject(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := legacyRoot["mcpServers"]; has {
		t.Errorf("legacy mcpServers.a2a not cleaned from settings.json: %v", legacyRoot)
	}
	if _, has := legacyRoot["hooks"]; has {
		t.Errorf("a2a hook not cleaned from settings.json: %v", legacyRoot)
	}
}
