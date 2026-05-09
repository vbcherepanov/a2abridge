package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kardianos/service"

	"github.com/vbcherepanov/a2abridge/internal/ideconfig"
)

func init() {
	registerCommand(Command{
		Name:    "uninstall",
		Summary: "Remove the MCP block from every IDE config, the skill, the hook and the service",
		Run:     RunUninstall,
	})
}

// RunUninstall reverses what `a2abridge install --apply` and
// `a2abridge service install` did. By default we keep .bak copies of
// every modified file. With --purge those are removed too along with
// ~/.a2abridge state.
func RunUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "also delete ~/.a2abridge state and remove all .bak files")
	keepService := fs.Bool("keep-service", false, "skip the service stop+uninstall step")
	dryRun := fs.Bool("dry-run", false, "describe what would be removed without touching anything")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge uninstall [flags]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Removes the a2a MCP block from every IDE config, deletes the skill and")
		fmt.Fprintln(stderr, "UserPromptSubmit hook, and stops + uninstalls the directory service.")
		fmt.Fprintln(stderr, "Each removed file is backed up to <name>.bak.<timestamp> unless --purge.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	failed := false

	// 1) Stop and uninstall the supervisor unit.
	if !*keepService {
		if err := uninstallService(stdout, stderr, *dryRun); err != nil {
			failed = true
		}
	}

	// 2) Strip the a2a MCP block from every detected IDE config.
	for _, w := range ideconfig.AllWriters() {
		path := w.Detect()
		if !ideconfig.WriterFound(w) {
			continue
		}
		if removeMCPBlock(stdout, stderr, w, path, *dryRun) {
			failed = true
		}
	}

	// 3) Remove skill, hook, .a2abridge state.
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "uninstall: home dir: %v\n", err)
		failed = true
	} else {
		removePath(stdout, stderr, filepath.Join(home, ".claude", "skills", "a2a-bridge"), "skill", *dryRun, *purge)
		removePath(stdout, stderr, filepath.Join(home, ".claude", "hooks", "a2a-inbox-hook.sh"), "hook", *dryRun, *purge)
		if *purge {
			removePath(stdout, stderr, filepath.Join(home, ".a2abridge"), "state dir", *dryRun, true)
		}
	}

	if *dryRun {
		fmt.Fprintln(stdout, "\nDry-run complete. Re-run without --dry-run to apply.")
	} else {
		fmt.Fprintln(stdout, "\nUninstall complete.")
	}
	if failed {
		return 1
	}
	return 0
}

// uninstallService stops + removes the kardianos/service unit. Errors
// are logged but not fatal — the user may have removed it manually.
func uninstallService(stdout, stderr io.Writer, dryRun bool) error {
	if dryRun {
		fmt.Fprintln(stdout, "  [plan]   service          stop + uninstall")
		return nil
	}
	svc, _, err := buildService(defaultDirectoryAddr)
	if err != nil {
		fmt.Fprintf(stderr, "  [WARN]  service          %v\n", err)
		return nil
	}
	st, _ := svc.Status()
	if st == service.StatusRunning {
		_ = svc.Stop()
	}
	if err := svc.Uninstall(); err != nil {
		fmt.Fprintf(stdout, "  [WARN]  service          uninstall: %v\n", err)
		return nil
	}
	fmt.Fprintln(stdout, "  [remove] service          stopped and uninstalled")
	return nil
}

// removeMCPBlock deletes the "a2a" key from the writer's config without
// disturbing any other servers the user has registered. We do not bother
// with strict idempotency — if the key isn't there, we say so and move on.
func removeMCPBlock(stdout, stderr io.Writer, w ideconfig.Writer, path string, dryRun bool) bool {
	if dryRun {
		fmt.Fprintf(stdout, "  [plan]   %-15s  %s — would strip a2a MCP block\n", w.Name(), path)
		return false
	}
	if err := ideconfig.RemoveMCPEntry(w, path); err != nil {
		fmt.Fprintf(stdout, "  [FAIL]   %-15s  %s — %v\n", w.Name(), path, err)
		return true
	}
	fmt.Fprintf(stdout, "  [remove] %-15s  %s\n", w.Name(), path)
	return false
}

// removePath deletes a file or directory tree. Without purge we rename
// to <path>.bak.<ts> so the user can restore. With purge we delete.
func removePath(stdout, stderr io.Writer, path, label string, dryRun, purge bool) {
	info, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "  [WARN]  %-15s  %v\n", label, err)
		}
		return
	}
	if dryRun {
		action := "rename to .bak"
		if purge {
			action = "delete"
		}
		fmt.Fprintf(stdout, "  [plan]   %-15s  %s — %s\n", label, path, action)
		return
	}
	if purge {
		if info.IsDir() {
			_ = os.RemoveAll(path)
		} else {
			_ = os.Remove(path)
		}
		fmt.Fprintf(stdout, "  [purge]  %-15s  %s\n", label, path)
		return
	}
	bak := path + ".bak." + time.Now().Format("20060102-150405")
	if err := os.Rename(path, bak); err != nil {
		fmt.Fprintf(stderr, "  [WARN]  %-15s  rename: %v\n", label, err)
		return
	}
	fmt.Fprintf(stdout, "  [backup] %-15s  %s → %s\n", label, path, filepath.Base(bak))
}
