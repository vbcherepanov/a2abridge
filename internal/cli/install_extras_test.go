package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vbcherepanov/a2abridge/v4/internal/assets"
)

func extrasHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestInstallSkillKeepsBackupsOutOfSkillsDir is the regression for the
// installer renaming the old skill to a2a-bridge.bak.<ts> next to the new
// one: Claude Code loads every directory under ~/.claude/skills, so the
// backup showed up as a duplicate skill.
func TestInstallSkillKeepsBackupsOutOfSkillsDir(t *testing.T) {
	home := extrasHome(t)
	skills := filepath.Join(home, ".claude", "skills")
	skillFile := filepath.Join(skills, "a2a-bridge", "SKILL.md")
	backups := filepath.Join(home, ".a2abridge", "backups")

	var out bytes.Buffer
	if err := installSkill(&out, true); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := os.Stat(skillFile); err != nil {
		t.Fatalf("skill not installed: %v", err)
	}

	// Re-running with nothing changed must not touch or back up anything.
	out.Reset()
	if err := installSkill(&out, true); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !strings.Contains(out.String(), "[skip]") {
		t.Errorf("unchanged skill was rewritten: %q", out.String())
	}
	if got := dirNames(t, backups); len(got) != 0 {
		t.Errorf("backup made for an unchanged skill: %v", got)
	}

	// A skill the user edited is backed up, outside ~/.claude/skills.
	const edited = "edited by the user\n"
	if err := os.WriteFile(skillFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := installSkill(&out, true); err != nil {
		t.Fatalf("third install: %v", err)
	}
	if got := dirNames(t, skills); len(got) != 1 || got[0] != "a2a-bridge" {
		t.Errorf("~/.claude/skills = %v, want only a2a-bridge", got)
	}
	saved := dirNames(t, backups)
	if len(saved) != 1 {
		t.Fatalf("backups = %v, want one", saved)
	}
	kept, err := os.ReadFile(filepath.Join(backups, saved[0], "SKILL.md"))
	if err != nil || string(kept) != edited {
		t.Errorf("backup does not hold the user's edit: %q, %v", kept, err)
	}
	src, err := assets.SkillFS()
	if err != nil {
		t.Fatal(err)
	}
	if same, err := assets.TreeMatches(src, filepath.Join(skills, "a2a-bridge")); err != nil || !same {
		t.Errorf("skill not restored to the embedded copy: same=%v err=%v", same, err)
	}
}

func TestInstallHookSkipsUnchanged(t *testing.T) {
	home := extrasHome(t)
	hooks := filepath.Join(home, ".claude", "hooks")

	var out bytes.Buffer
	if err := installHook(&out, true); err != nil {
		t.Fatalf("first install: %v", err)
	}
	out.Reset()
	if err := installHook(&out, true); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if !strings.Contains(out.String(), "[skip]") {
		t.Errorf("unchanged hook was rewritten: %q", out.String())
	}
	if got := dirNames(t, hooks); len(got) != 1 {
		t.Errorf("~/.claude/hooks = %v, want only the hook", got)
	}

	if err := os.WriteFile(filepath.Join(hooks, "a2a-inbox-hook.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installHook(&out, true); err != nil {
		t.Fatalf("third install: %v", err)
	}
	if got := dirNames(t, hooks); len(got) != 2 {
		t.Errorf("~/.claude/hooks = %v, want the hook and one backup", got)
	}
}

func TestUninstallMovesSkillToBackups(t *testing.T) {
	home := extrasHome(t)
	var out, errOut bytes.Buffer
	if err := installSkill(&out, true); err != nil {
		t.Fatal(err)
	}
	removeSkill(&out, &errOut, home, false, false)

	if got := dirNames(t, filepath.Join(home, ".claude", "skills")); len(got) != 0 {
		t.Errorf("~/.claude/skills = %v, want empty", got)
	}
	if got := dirNames(t, filepath.Join(home, ".a2abridge", "backups")); len(got) != 1 {
		t.Errorf("backups = %v, want one", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected warnings: %s", errOut.String())
	}
}

// TestDoctorAcceptsCustomisedEntry is the regression for doctor reporting
// "missing or stale" right after a clean install, because it compared the
// entry literally against the defaults.
func TestDoctorAcceptsCustomisedEntry(t *testing.T) {
	home := extrasHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := RunInstall([]string{"--apply", "--ide", "claude-code"}, &out, &errOut); code != 0 {
		t.Fatalf("install exit = %d\n%s%s", code, out.String(), errOut.String())
	}

	// Add what a user would: a transport type and their own env var.
	path := filepath.Join(home, ".claude.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `"A2A_BIND"`, `"A2A_MODEL": "some-model", "A2A_BIND"`, 1)
	if edited == string(raw) {
		t.Fatalf("fixture edit did not apply:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, r := range checkIDEs() {
		if r.Name == "ide:Claude Code" && r.Status != "PASS" {
			t.Errorf("doctor reports %s for a customised entry: %s", r.Status, r.Detail)
		}
	}
}
