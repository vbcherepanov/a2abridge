package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/assets"
	"github.com/vbcherepanov/a2abridge/internal/ideconfig"
)

func init() {
	registerCommand(Command{
		Name:    "install",
		Summary: "Auto-detect IDEs and register the a2abridge MCP server in each one",
		Run:     RunInstall,
	})
}

// RunInstall is the user-facing installer.
//
//	a2abridge install                # auto-detect, dry-run by default
//	a2abridge install --apply        # actually write
//	a2abridge install --ide claude-code,codex --apply
//	a2abridge install --binary /custom/path --apply
//
// Default is dry-run because writing into ~/.claude/settings.json is
// irreversible from the user's perspective (yes, we backup, but we still
// don't want surprise edits).
func RunInstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "actually write the configs (default is dry-run)")
	dryRun := fs.Bool("dry-run", false, "force dry-run even if --apply is set")
	ideFlag := fs.String("ide", "auto", "comma-separated IDE list (auto|claude-code,codex,cursor,cline,continue,gemini)")
	binary := fs.String("binary", "", "absolute path to the a2abridge binary (default: this executable)")
	directory := fs.String("directory", "http://127.0.0.1:7777", "directory URL written into each config")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge install [flags]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Detects supported IDEs on this machine and writes the a2a MCP block")
		fmt.Fprintln(stderr, "into each one's config, with a timestamped .bak backup. Default mode is")
		fmt.Fprintln(stderr, "dry-run (no files modified) — pass --apply to actually write.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *dryRun {
		*apply = false
	}

	// Resolve the binary path: caller can override; otherwise use our own.
	exe := *binary
	if exe == "" {
		me, err := os.Executable()
		if err != nil {
			fmt.Fprintf(stderr, "install: locate own binary: %v\n", err)
			return 1
		}
		// Prefer the symlink-resolved path so configs survive directory moves.
		if real, rerr := filepath.EvalSymlinks(me); rerr == nil {
			me = real
		}
		exe = me
	}

	spec := ideconfig.DefaultSpec(exe)
	spec.Env["A2A_DIRECTORY"] = *directory
	if home, herr := os.UserHomeDir(); herr == nil {
		// Wire the embedded hook into Claude Code's UserPromptSubmit. The
		// hook itself is copied later by installHook(); here we just point
		// settings.json at the canonical location so settings + hook land
		// in the same backup window.
		spec.HookCommand = filepath.Join(home, ".claude", "hooks", "a2a-inbox-hook.sh")
	}

	wantedIDEs := parseIDEFilter(*ideFlag)
	all := ideconfig.AllWriters()
	selected := filterWriters(all, wantedIDEs)
	if len(selected) == 0 {
		fmt.Fprintf(stderr, "install: no matching IDEs (filter=%q)\n", *ideFlag)
		return 1
	}

	mode := "dry-run"
	if *apply {
		mode = "apply"
	}
	fmt.Fprintf(stdout, "a2abridge install (%s)\n", mode)
	fmt.Fprintf(stdout, "  binary:    %s\n", exe)
	fmt.Fprintf(stdout, "  directory: %s\n\n", *directory)

	results := make([]ideconfig.Result, 0, len(selected))
	failed := false
	for _, w := range selected {
		// auto-mode quietly skips IDEs whose config files don't exist yet:
		// the user clearly hasn't installed those IDEs, no point creating
		// stale skeletons. Explicit --ide=foo opts in.
		if *ideFlag == "auto" && !ideconfig.WriterFound(w) {
			fmt.Fprintf(stdout, "  [skip]   %-15s — not detected\n", w.Name())
			continue
		}
		res := w.Write(spec, !*apply)
		results = append(results, res)

		switch {
		case res.Error != nil:
			fmt.Fprintf(stdout, "  [FAIL]   %-15s  %s — %v\n", res.IDE, res.Path, res.Error)
			failed = true
		case res.Skipped:
			fmt.Fprintf(stdout, "  [skip]   %-15s  %s — already up to date\n", res.IDE, res.Path)
		case res.Updated && !*apply:
			fmt.Fprintf(stdout, "  [plan]   %-15s  %s — would write a2a MCP block\n", res.IDE, res.Path)
		case res.Updated && *apply:
			if res.Backup != "" {
				fmt.Fprintf(stdout, "  [write]  %-15s  %s  (backup → %s)\n", res.IDE, res.Path, filepath.Base(res.Backup))
			} else {
				fmt.Fprintf(stdout, "  [write]  %-15s  %s  (created)\n", res.IDE, res.Path)
			}
		}
	}

	// Skill + hook install (Claude Code only). Both have backup-on-overwrite
	// semantics so re-running install --apply doesn't clobber user edits.
	skillFailed := false
	if shouldInstallExtras(*ideFlag) {
		if err := installSkill(stdout, *apply); err != nil {
			fmt.Fprintf(stdout, "  [FAIL]   skill            %v\n", err)
			skillFailed = true
		}
		if err := installHook(stdout, *apply); err != nil {
			fmt.Fprintf(stdout, "  [FAIL]   hook             %v\n", err)
			skillFailed = true
		}
	}

	if !*apply {
		fmt.Fprintln(stdout, "\nDry-run complete. Re-run with --apply to actually write.")
	} else {
		fmt.Fprintln(stdout, "\nInstall complete. Restart the listed IDEs to pick up the new MCP server.")
	}

	if failed || skillFailed {
		return 1
	}
	return 0
}

// shouldInstallExtras reports whether the skill + hook should be touched
// for this --ide selection. We install them whenever Claude Code is in
// scope (default) and skip if the user explicitly excluded it via --ide.
func shouldInstallExtras(ideFlag string) bool {
	s := strings.ToLower(strings.TrimSpace(ideFlag))
	if s == "" || s == "auto" || s == "all" {
		return true
	}
	for _, p := range strings.Split(s, ",") {
		if strings.TrimSpace(p) == "claude-code" {
			return true
		}
	}
	return false
}

// installSkill drops the embedded skill into ~/.claude/skills/a2a-bridge/.
// Backups: any pre-existing file at the destination is renamed to
// `<name>.bak.<timestamp>` before it gets overwritten.
func installSkill(stdout io.Writer, apply bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	target := filepath.Join(home, ".claude", "skills", "a2a-bridge")
	src, err := assets.SkillFS()
	if err != nil {
		return fmt.Errorf("embed skill fs: %w", err)
	}

	if !apply {
		fmt.Fprintf(stdout, "  [plan]   skill            %s\n", target)
		return nil
	}

	if err := backupTreeOnce(target); err != nil {
		return fmt.Errorf("backup skill tree: %w", err)
	}
	if err := assets.CopyTree(src, target); err != nil {
		return fmt.Errorf("copy skill: %w", err)
	}
	fmt.Fprintf(stdout, "  [write]  skill            %s\n", target)
	return nil
}

// installHook copies the embedded UserPromptSubmit hook into
// ~/.claude/hooks/a2a-inbox-hook.sh with execute permission.
func installHook(stdout io.Writer, apply bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home dir: %w", err)
	}
	target := filepath.Join(home, ".claude", "hooks", "a2a-inbox-hook.sh")

	if !apply {
		fmt.Fprintf(stdout, "  [plan]   hook             %s\n", target)
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, statErr := os.Stat(target); statErr == nil {
		bak := target + ".bak." + time.Now().Format("20060102-150405")
		if err := os.Rename(target, bak); err != nil {
			return fmt.Errorf("backup existing hook: %w", err)
		}
	}
	if err := os.WriteFile(target, assets.HookScript(), 0o755); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "  [write]  hook             %s\n", target)
	return nil
}

// backupTreeOnce renames the entire target directory to `<dir>.bak.<ts>`
// the first time we touch it, so the original skill is preserved as a
// single tree (rather than per-file backups scattered across references/).
// A no-op when the target does not exist.
func backupTreeOnce(target string) error {
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		// Unlikely, but if a file is in the way, rename it out of the way.
		bak := target + ".bak." + time.Now().Format("20060102-150405")
		return os.Rename(target, bak)
	}
	bak := target + ".bak." + time.Now().Format("20060102-150405")
	return os.Rename(target, bak)
}

// parseIDEFilter normalises the --ide value to a set of writer names
// (lowercased). "auto" is an empty set and means "every detected writer".
func parseIDEFilter(s string) map[string]bool {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "auto" || s == "all" {
		return nil
	}
	out := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out[p] = true
	}
	return out
}

// filterWriters returns the subset of writers matching wanted. If wanted
// is nil, returns all of them (sorted by display name).
func filterWriters(all []ideconfig.Writer, wanted map[string]bool) []ideconfig.Writer {
	if wanted == nil {
		out := make([]ideconfig.Writer, len(all))
		copy(out, all)
		sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
		return out
	}
	out := make([]ideconfig.Writer, 0, len(wanted))
	for _, w := range all {
		if wanted[writerSlug(w)] {
			out = append(out, w)
		}
	}
	return out
}

// writerSlug converts the IDE display name into a short cli slug:
//
//	"Claude Code"        → "claude-code"
//	"Codex CLI"          → "codex"        (the " CLI" suffix is dropped)
//	"Gemini CLI"         → "gemini"
//	"Cline (VS Code)"    → "cline"        (parenthesised suffixes dropped)
//	"Continue" / "Cursor"→ as-is, lowercased
//
// Keeping the slugs single-word is what lets users type --ide codex,gemini
// without having to remember the formal display name.
func writerSlug(w ideconfig.Writer) string {
	s := strings.ToLower(w.Name())
	if i := strings.Index(s, "("); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimSuffix(s, " cli")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}
