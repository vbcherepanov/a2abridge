package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/vbcherepanov/a2abridge/internal/ideconfig"
)

func TestParseIDEFilter(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]bool
	}{
		{"", nil},
		{"auto", nil},
		{"all", nil},
		{"claude-code", map[string]bool{"claude-code": true}},
		{"claude-code,codex", map[string]bool{"claude-code": true, "codex": true}},
		{"  Claude-Code , CODEX  ", map[string]bool{"claude-code": true, "codex": true}},
	}
	for _, c := range cases {
		got := parseIDEFilter(c.in)
		if !mapEq(got, c.want) {
			t.Errorf("parseIDEFilter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func mapEq(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestWriterSlugCanonical(t *testing.T) {
	cases := map[string]string{
		"Claude Code":      "claude-code",
		"Codex CLI":        "codex",
		"Cursor":           "cursor",
		"Cline (VS Code)":  "cline",
		"Continue":         "continue",
		"Gemini CLI":       "gemini",
	}
	for _, w := range ideconfig.AllWriters() {
		want, ok := cases[w.Name()]
		if !ok {
			continue
		}
		got := writerSlug(w)
		if got != want {
			t.Errorf("writerSlug(%q) = %q, want %q", w.Name(), got, want)
		}
	}
}

func TestShouldInstallExtras(t *testing.T) {
	cases := map[string]bool{
		"":                 true,  // auto
		"auto":             true,
		"all":              true,
		"claude-code":      true,
		"codex":            false, // claude-code not in scope
		"claude-code,codex": true,
		"codex,cursor":     false,
	}
	for in, want := range cases {
		if got := shouldInstallExtras(in); got != want {
			t.Errorf("shouldInstallExtras(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRunDispatchUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing hint: %q", stderr.String())
	}
}

func TestRunDispatchHelpFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("--help exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Commands:") {
		t.Errorf("--help stdout did not list commands: %q", stdout.String())
	}
}

func TestRunDispatchVersionFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Errorf("--version exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "platform:") {
		t.Errorf("--version stdout missing platform line: %q", stdout.String())
	}
}

func TestCompletionGeneratesAllShells(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var stdout, stderr bytes.Buffer
		code := RunCompletion([]string{shell}, &stdout, &stderr)
		if code != 0 {
			t.Errorf("completion %s exit=%d stderr=%q", shell, code, stderr.String())
			continue
		}
		out := stdout.String()
		// Every shell template must mention 'install' since that's a registered command.
		if !strings.Contains(out, "install") {
			t.Errorf("completion %s missing 'install' in output:\n%s", shell, out)
		}
		// And must NOT duplicate 'version' (we sort+dedup before render).
		if strings.Count(out, " version") > 2 {
			t.Errorf("completion %s seems to duplicate version:\n%s", shell, out)
		}
	}
}
