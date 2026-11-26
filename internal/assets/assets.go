// Package assets embeds the skill and hook files distributed with the
// a2abridge binary. This way the single binary can ship + install the
// Claude Code skill on any machine without needing the source tree.
package assets

import (
	"bytes"
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:skill
var skillFS embed.FS

//go:embed hook/a2a-inbox-hook.sh
var hookBytes []byte

// SkillFS returns a sub-FS rooted at the skill directory so callers see
// "SKILL.md" / "references/protocol.md" rather than "skill/SKILL.md".
func SkillFS() (fs.FS, error) {
	return fs.Sub(skillFS, "skill")
}

// HookScript returns the bytes of the UserPromptSubmit hook script.
func HookScript() []byte { return hookBytes }

// CopyTree extracts all files from src into dst, preserving relative
// paths and ensuring parent directories exist. Existing files are
// overwritten — the caller must handle backup/idempotency upstream.
func CopyTree(src fs.FS, dst string) error {
	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == "." {
				return os.MkdirAll(dst, 0o755)
			}
			return os.MkdirAll(filepath.Join(dst, p), 0o755)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, p)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		// Tag executables (.sh) with execute bit; everything else gets 0644.
		mode := os.FileMode(0o644)
		if strings.HasSuffix(p, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(out, data, mode)
	})
}

// TreeMatches reports whether dst holds exactly the files of src with the
// same contents: nothing missing, nothing changed, nothing extra. A dst
// that does not exist does not match.
func TreeMatches(src fs.FS, dst string) (bool, error) {
	want := map[string]bool{}
	same := true
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		want[filepath.ToSlash(p)] = true
		expected, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(p)))
		if err != nil {
			if os.IsNotExist(err) {
				same = false
				return fs.SkipAll
			}
			return err
		}
		if !bytes.Equal(expected, got) {
			same = false
			return fs.SkipAll
		}
		return nil
	})
	if err != nil || !same {
		return false, err
	}

	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		if !want[filepath.ToSlash(rel)] {
			same = false
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return same, nil
}
