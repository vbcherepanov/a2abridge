package assets

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSkillFSContainsExpectedFiles guards against accidentally dropping
// files from the embed graph during refactors. The skill must always
// ship at least SKILL.md — the rest of `references/` is bonus.
func TestSkillFSContainsExpectedFiles(t *testing.T) {
	src, err := SkillFS()
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	if err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			found = append(found, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !contains(found, "SKILL.md") {
		t.Errorf("SkillFS missing SKILL.md (have %v)", found)
	}
}

func TestHookScriptStartsWithShebang(t *testing.T) {
	body := HookScript()
	if len(body) == 0 {
		t.Fatal("hook script is empty")
	}
	if !strings.HasPrefix(string(body), "#!") {
		t.Errorf("hook script missing shebang: %q", string(body[:min(len(body), 40)]))
	}
}

// TestCopyTreeMarksShellExecutable verifies that when CopyTree extracts
// a .sh file, the result is mode 0755 — otherwise the UserPromptSubmit
// hook would land non-executable on the user's disk.
func TestCopyTreeMarksShellExecutable(t *testing.T) {
	dst := t.TempDir()
	src, err := SkillFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}
	// SkillFS doesn't ship .sh, so synthesize one and re-extract through
	// the same CopyTree path using a fake fs.FS.
	fakeFS := os.DirFS("testdata-not-needed-just-skip")
	_ = fakeFS

	// Direct check: write a .sh to dst, mirror CopyTree's logic by
	// invoking it on a tmp source dir.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "foo.sh"), []byte("#!/bin/sh\necho hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(os.DirFS(srcDir), dst); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dst, "foo.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("CopyTree produced non-executable .sh: mode = %v", info.Mode().Perm())
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
