// Package ideconfig knows how to inject the a2abridge MCP server block into
// each supported IDE's configuration file. Every writer is responsible for:
//
//	1. locating its config file across macOS / Linux / Windows / WSL2;
//	2. creating it if missing (with a sensible skeleton);
//	3. preserving every other key the user has set;
//	4. timestamped .bak backup before any write;
//	5. round-tripping comments and existing format as much as the underlying
//	   serialiser allows (json round-trip drops comments — we accept that).
//
// New IDEs are added by implementing the Writer interface and appending to
// AllWriters.
package ideconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Spec describes the MCP entry to inject.
type Spec struct {
	// Key is the name peers / the IDE will see. Always "a2a" — keeping it as
	// a field anyway in case of future namespacing.
	Key string
	// BinaryPath is the absolute path to the a2abridge binary. The bridge
	// subcommand will be appended automatically by each writer.
	BinaryPath string
	// Env is the environment block injected into the MCP server entry. Order
	// is preserved by writers where the format supports ordered maps.
	Env map[string]string
	// HookCommand — when non-empty, Claude Code's writer registers it as a
	// UserPromptSubmit hook in the same settings.json transaction. Other
	// IDEs ignore this field (they have no equivalent concept).
	HookCommand string
}

// DefaultSpec returns the canonical environment a fresh installer wants
// to write. binaryPath should be the absolute path to a2abridge.
func DefaultSpec(binaryPath string) Spec {
	return Spec{
		Key:        "a2a",
		BinaryPath: binaryPath,
		Env: map[string]string{
			"A2A_DIRECTORY":      "http://127.0.0.1:7777",
			"A2A_BIND":           "127.0.0.1:0",
			"A2A_ADVERTISE_HOST": "127.0.0.1",
		},
	}
}

// Result captures what one writer did so the installer can summarise.
type Result struct {
	IDE       string // human-readable IDE name
	Path      string // resolved config path (may be "" if not found)
	Found     bool   // config file existed before this run
	Updated   bool   // we wrote a new MCP block (or rewrote a stale one)
	Skipped   bool   // we found the config but the MCP block was already up to date
	Backup    string // path to the .bak we created (empty if no write)
	DryRun    bool
	Error     error
}

// Writer abstracts each IDE-specific integration.
type Writer interface {
	// Name returns a human label like "Claude Code".
	Name() string
	// Detect returns the resolved config path if this IDE looks installed
	// on the current machine. Empty path = not installed (or not detected).
	Detect() string
	// Write injects the MCP block. If dryRun is true, no file is touched —
	// Result.Updated still reflects what *would* have been written.
	Write(spec Spec, dryRun bool) Result
}

// AllWriters returns one writer per supported IDE, in display order.
func AllWriters() []Writer {
	return []Writer{
		&claudeCodeWriter{},
		&codexWriter{},
		&cursorWriter{},
		&clineWriter{},
		&continueWriter{},
		&geminiWriter{},
	}
}

// WriterFound reports whether the writer's config file exists on disk.
// Used by `a2abridge install` to skip IDEs that aren't installed when
// running in --ide=auto mode.
func WriterFound(w Writer) bool {
	return fileExists(w.Detect())
}

// RemoveMCPEntry strips the "a2a" key from the writer's config — used
// by `a2abridge uninstall`. The implementation is writer-agnostic for
// JSON-based IDEs (Claude Code, Cursor, Cline, Gemini); Codex (TOML) and
// Continue (own file) are handled by special cases below.
func RemoveMCPEntry(w Writer, path string) error {
	if path == "" {
		return nil
	}
	switch w.(type) {
	case *codexWriter:
		return removeCodexEntry(path)
	case *continueWriter:
		return removeContinueFile(path)
	default:
		return removeJSONMCPEntry(path)
	}
}

// homeDir is a small wrapper for predictable testing.
var homeDir = func() (string, error) {
	return os.UserHomeDir()
}

// expandHome resolves a path that may start with "~". It does NOT expand
// shell variables — that's the IDE's job, not ours.
func expandHome(p string) (string, error) {
	if len(p) == 0 || p[0] != '~' {
		return p, nil
	}
	h, err := homeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return h, nil
	}
	return filepath.Join(h, p[1:]), nil
}

// backupFile copies src to src + ".bak.YYYYMMDD-HHMMSS" when src exists.
// Returns the backup path or "" if there was nothing to back up.
func backupFile(src string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("backup target is a directory: %s", src)
	}
	stamp := time.Now().Format("20060102-150405")
	dst := src + ".bak." + stamp
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
		return "", err
	}
	return dst, nil
}

// fileExists is a tiny helper used by every Detect implementation.
func fileExists(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// VSCodeUserSettingsPath resolves the per-user VS Code settings.json across
// platforms. Returns "" if the standard install layout is not detected.
func VSCodeUserSettingsPath() string {
	switch runtime.GOOS {
	case "darwin":
		if h, err := homeDir(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "Code", "User", "settings.json")
		}
	case "linux":
		if h, err := homeDir(); err == nil {
			return filepath.Join(h, ".config", "Code", "User", "settings.json")
		}
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Code", "User", "settings.json")
		}
	}
	return ""
}

// platformExeSuffix returns ".exe" on Windows so callers building an
// argv[0] reference don't have to duplicate the runtime.GOOS check.
func platformExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
