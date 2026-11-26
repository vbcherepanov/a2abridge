package ideconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

const testBinary = "/opt/a2abridge/bin/a2abridge"

func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestClaudeWriteKeepsUserKeys is the regression for install --apply
// replacing the whole a2a entry: a transport type and custom env vars the
// user added by hand were gone after an upgrade.
func TestClaudeWriteKeepsUserKeys(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".claude.json")
	writeTestFile(t, path, `{
  "mcpServers": {
    "a2a": {
      "type": "stdio",
      "command": "/old/a2abridge",
      "args": ["bridge"],
      "env": {
        "A2A_DIRECTORY": "http://10.0.0.5:7777",
        "A2A_MODEL": "some-model",
        "A2A_SKILLS": "go,php"
      }
    }
  }
}`)

	res := claudeCodeWriter{}.Write(DefaultSpec(testBinary), false)
	if res.Error != nil || !res.Updated {
		t.Fatalf("Write = %+v, want Updated", res)
	}

	root, err := readJSONObject(path)
	if err != nil {
		t.Fatal(err)
	}
	entry := root["mcpServers"].(map[string]any)["a2a"].(map[string]any)
	if entry["command"] != testBinary {
		t.Errorf("command = %v, want %s", entry["command"], testBinary)
	}
	if entry["type"] != "stdio" {
		t.Errorf("type = %v, want it kept as stdio", entry["type"])
	}
	env := entry["env"].(map[string]any)
	want := map[string]string{
		"A2A_DIRECTORY":      "http://10.0.0.5:7777", // the user's value, not the default
		"A2A_MODEL":          "some-model",
		"A2A_SKILLS":         "go,php",
		"A2A_BIND":           "127.0.0.1:0", // filled in, it was missing
		"A2A_ADVERTISE_HOST": "127.0.0.1",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("env[%s] = %v, want %s", k, env[k], v)
		}
	}

	// doctor runs the same writer in dry-run with the default spec: a
	// hand-tuned entry must read as up to date, not stale.
	if again := (claudeCodeWriter{}).Write(DefaultSpec(testBinary), true); !again.Skipped {
		t.Errorf("second Write = %+v, want Skipped", again)
	}
}

func TestMergeForceEnvReplacesUserValue(t *testing.T) {
	spec := DefaultSpec(testBinary)
	spec.Env["A2A_DIRECTORY"] = "http://127.0.0.1:9999"
	spec.ForceEnv = map[string]bool{"A2A_DIRECTORY": true}

	got := mergeMCPEntry(map[string]any{
		"args": []any{"bridge", "--log-level", "debug"},
		"env":  map[string]any{"A2A_DIRECTORY": "http://old:7777", "A2A_BIND": "0.0.0.0:7801"},
	}, spec)

	env := got["env"].(map[string]any)
	if env["A2A_DIRECTORY"] != "http://127.0.0.1:9999" {
		t.Errorf("forced A2A_DIRECTORY = %v", env["A2A_DIRECTORY"])
	}
	if env["A2A_BIND"] != "0.0.0.0:7801" {
		t.Errorf("A2A_BIND = %v, want the user's value kept", env["A2A_BIND"])
	}
	if args := got["args"].([]any); len(args) != 3 {
		t.Errorf("args = %v, want the user's extra flags kept", args)
	}

	if args := mergeMCPEntry(map[string]any{"args": []any{"serve"}}, spec)["args"].([]any); len(args) != 1 || args[0] != "bridge" {
		t.Errorf("args = %v, want [bridge] when they did not start the bridge", args)
	}
}

const codexConfig = `# Codex settings, edited by hand
model = "gpt-5"

# >>> memory server (managed block, do not edit)
[mcp_servers.memory]
command = "/usr/local/bin/tam"
# <<< memory server

[mcp_servers.a2a]
command = "/old/a2abridge" # previous install
args = ["bridge"]

[mcp_servers.a2a.env]
A2A_DIRECTORY = "http://10.0.0.5:7777"
A2A_MODEL = "some-model"

# approvals for the bridge tools
[mcp_servers.a2a.tools.a2a_inbox]
approval_mode = "approve"

[profiles.fast]
model = "gpt-5-mini"
`

// TestCodexWritePatchesInPlace is the regression for install --apply
// re-encoding the whole config.toml: comments, key order, custom env vars
// and the tool approval tables under the a2a entry were all lost.
func TestCodexWritePatchesInPlace(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeTestFile(t, path, codexConfig)

	res := codexWriter{}.Write(DefaultSpec(testBinary), false)
	if res.Error != nil || !res.Updated {
		t.Fatalf("Write = %+v, want Updated", res)
	}
	if res.Note != "" {
		t.Errorf("Note = %q, want an in-place patch", res.Note)
	}

	want := strings.NewReplacer(
		`command = "/old/a2abridge" # previous install`, `command = "`+testBinary+`"`,
		"A2A_MODEL = \"some-model\"\n", "A2A_MODEL = \"some-model\"\nA2A_ADVERTISE_HOST = \"127.0.0.1\"\nA2A_BIND = \"127.0.0.1:0\"\n",
	).Replace(codexConfig)
	if got := readTestFile(t, path); got != want {
		t.Errorf("config.toml after install:\n%s\nwant:\n%s", got, want)
	}

	if again := (codexWriter{}).Write(DefaultSpec(testBinary), false); !again.Skipped {
		t.Errorf("second Write = %+v, want Skipped", again)
	}
}

func TestCodexWriteAddsEntryToExistingFile(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	const before = "# my settings\nmodel = \"gpt-5\"\n\n[mcp_servers.memory]\ncommand = \"tam\" # keep\n"
	writeTestFile(t, path, before)

	res := codexWriter{}.Write(DefaultSpec(testBinary), false)
	if res.Error != nil || res.Note != "" {
		t.Fatalf("Write = %+v", res)
	}
	got := readTestFile(t, path)
	if !strings.HasPrefix(got, before) {
		t.Errorf("existing content changed:\n%s", got)
	}
	assertCodexEntry(t, path, testBinary, map[string]string{
		"A2A_DIRECTORY": "http://127.0.0.1:7777", "A2A_BIND": "127.0.0.1:0", "A2A_ADVERTISE_HOST": "127.0.0.1",
	})
}

func TestCodexWriteCreatesMissingFile(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if res := (codexWriter{}).Write(DefaultSpec(testBinary), false); res.Error != nil || !res.Updated {
		t.Fatalf("Write = %+v", res)
	}
	assertCodexEntry(t, path, testBinary, map[string]string{"A2A_BIND": "127.0.0.1:0"})
}

func TestCodexWriteInlineEnv(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeTestFile(t, path, "[mcp_servers.a2a]\ncommand = \""+testBinary+"\"\nargs = [\"bridge\"]\nenv = { A2A_MODEL = \"m\" } # inline\n\n[other]\nk = 1 # keep\n")

	res := codexWriter{}.Write(DefaultSpec(testBinary), false)
	if res.Error != nil || res.Note != "" {
		t.Fatalf("Write = %+v", res)
	}
	got := readTestFile(t, path)
	if !strings.Contains(got, "k = 1 # keep") || strings.Contains(got, "[mcp_servers.a2a.env]") {
		t.Errorf("inline env not patched in place:\n%s", got)
	}
	assertCodexEntry(t, path, testBinary, map[string]string{"A2A_MODEL": "m", "A2A_BIND": "127.0.0.1:0"})
}

// TestCodexWriteFallsBackToReencode covers a layout the line patcher does
// not understand: the result must still be correct, and the user is told
// the file was re-encoded.
func TestCodexWriteFallsBackToReencode(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeTestFile(t, path, "[mcp_servers]\na2a = { command = \"/old\", args = [\"bridge\"], env = { A2A_MODEL = \"m\" } }\n")

	res := codexWriter{}.Write(DefaultSpec(testBinary), false)
	if res.Error != nil || !res.Updated {
		t.Fatalf("Write = %+v", res)
	}
	if res.Note == "" || res.Backup == "" {
		t.Errorf("fallback must set Note and keep a backup, got %+v", res)
	}
	assertCodexEntry(t, path, testBinary, map[string]string{"A2A_MODEL": "m", "A2A_BIND": "127.0.0.1:0"})
}

func TestRemoveCodexEntryKeepsRestOfFile(t *testing.T) {
	home := tempHome(t)
	path := filepath.Join(home, ".codex", "config.toml")
	writeTestFile(t, path, codexConfig)

	if err := removeCodexEntry(path); err != nil {
		t.Fatal(err)
	}
	const want = `# Codex settings, edited by hand
model = "gpt-5"

# >>> memory server (managed block, do not edit)
[mcp_servers.memory]
command = "/usr/local/bin/tam"
# <<< memory server

# approvals for the bridge tools

[profiles.fast]
model = "gpt-5-mini"
`
	if got := readTestFile(t, path); got != want {
		t.Errorf("config.toml after uninstall:\n%s\nwant:\n%s", got, want)
	}
}

func assertCodexEntry(t *testing.T, path, command string, env map[string]string) {
	t.Helper()
	var root map[string]any
	if _, err := toml.DecodeFile(path, &root); err != nil {
		t.Fatalf("result is not valid TOML: %v\n%s", err, readTestFile(t, path))
	}
	entry, _ := root["mcp_servers"].(map[string]any)["a2a"].(map[string]any)
	if entry == nil {
		t.Fatalf("mcp_servers.a2a missing:\n%s", readTestFile(t, path))
	}
	if entry["command"] != command {
		t.Errorf("command = %v, want %s", entry["command"], command)
	}
	if args, _ := entry["args"].([]any); len(args) != 1 || args[0] != "bridge" {
		t.Errorf("args = %v, want [bridge]", entry["args"])
	}
	got, _ := entry["env"].(map[string]any)
	for k, v := range env {
		if got[k] != v {
			t.Errorf("env[%s] = %v, want %s", k, got[k], v)
		}
	}
}
