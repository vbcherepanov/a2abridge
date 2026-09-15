package ideconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func antigravityHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestAntigravityDetectsStateDirBeforeConfigExists: a fresh agy install has
// no mcp_config.json yet, so auto-install must still find it.
func TestAntigravityDetectsStateDirBeforeConfigExists(t *testing.T) {
	home := antigravityHome(t)
	w := &antigravityWriter{}
	if WriterFound(w) {
		t.Fatalf("detected without any Antigravity state; Detect() = %q", w.Detect())
	}
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !WriterFound(w) {
		t.Fatalf("not detected with ~/.gemini/antigravity-cli present; Detect() = %q", w.Detect())
	}
}

// TestAntigravityWriteCreatesConfig: the first write creates
// ~/.gemini/config/mcp_config.json and leaves Gemini CLI's settings alone.
func TestAntigravityWriteCreatesConfig(t *testing.T) {
	home := antigravityHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(home, ".gemini", "config", "mcp_config.json")

	w := &antigravityWriter{}
	res := w.Write(DefaultSpec("/opt/a2abridge"), false)
	if res.Error != nil {
		t.Fatalf("Write: %v", res.Error)
	}
	if !res.Updated || res.Path != target {
		t.Fatalf("Write result = %+v, want Updated at %s", res, target)
	}
	root, err := readJSONObject(target)
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	entry, _ := servers["a2a"].(map[string]any)
	if entry == nil || entry["command"] != "/opt/a2abridge" {
		t.Fatalf("mcpServers.a2a = %v, want command /opt/a2abridge", servers["a2a"])
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("Gemini CLI settings.json was touched (stat err = %v)", err)
	}
	if w.Detect() != target {
		t.Errorf("Detect() after write = %q, want %s", w.Detect(), target)
	}
}

// TestAntigravityPreservesServersAndUninstalls: other servers survive both
// install and uninstall, and a second install is a no-op.
func TestAntigravityPreservesServersAndUninstalls(t *testing.T) {
	home := antigravityHome(t)
	target := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{"mcpServers":{"cloudrun":{"command":"npx","args":["-y","@google-cloud/cloud-run-mcp"]}}}`
	if err := os.WriteFile(target, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	w := &antigravityWriter{}
	spec := DefaultSpec("/opt/a2abridge")
	if res := w.Write(spec, false); res.Error != nil || !res.Updated {
		t.Fatalf("first Write = %+v", res)
	}
	if res := w.Write(spec, false); res.Error != nil || !res.Skipped {
		t.Errorf("second Write expected Skipped, got %+v", res)
	}

	if err := RemoveMCPEntry(w, w.Detect()); err != nil {
		t.Fatalf("RemoveMCPEntry: %v", err)
	}
	root, err := readJSONObject(target)
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if _, has := servers["a2a"]; has {
		t.Error("a2a still present after uninstall")
	}
	if _, has := servers["cloudrun"]; !has {
		t.Error("unrelated server removed")
	}
}

// TestAntigravityRemoveWithOnlyStateDir: Detect returns the state directory
// when no config exists; uninstall must not try to parse it as JSON.
func TestAntigravityRemoveWithOnlyStateDir(t *testing.T) {
	home := antigravityHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".gemini", "antigravity-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := &antigravityWriter{}
	if err := RemoveMCPEntry(w, w.Detect()); err != nil {
		t.Fatalf("RemoveMCPEntry with only the state directory: %v", err)
	}
}
