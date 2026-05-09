package ideconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// readJSONObject reads path as JSON (or JSON-with-comments) and returns
// its top-level object. If the file is missing or empty, returns an empty
// map. We accept JSONC because Claude Code, Cursor, Gemini CLI and other
// editors all let users keep // and /* */ comments in their settings
// files. The comments are stripped before parsing — they will be lost on
// rewrite, which is fine because we always create a timestamped .bak
// before any write.
func readJSONObject(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	cleaned := stripJSONComments(b)
	cleaned = stripTrailingCommas(cleaned)
	var m map[string]any
	if err := json.Unmarshal(cleaned, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// stripJSONComments removes // line comments and /* */ block comments,
// keeping byte offsets stable so error messages stay close to the source
// (we replace comment bytes with spaces of the same length, except for
// newlines which we preserve so line numbers don't shift).
//
// String literals are respected: a // inside a quoted string is left
// alone. Backslash escapes inside strings are honoured so that "\\"
// followed by a quote does not falsely close the string.
func stripJSONComments(in []byte) []byte {
	out := make([]byte, len(in))
	copy(out, in)

	n := len(out)
	i := 0
	inString := false
	for i < n {
		c := out[i]
		if inString {
			if c == '\\' && i+1 < n {
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
			i++
			continue
		}
		if c == '"' {
			inString = true
			i++
			continue
		}
		if c == '/' && i+1 < n {
			next := out[i+1]
			if next == '/' {
				// // line comment — blank to end of line
				for j := i; j < n && out[j] != '\n'; j++ {
					out[j] = ' '
				}
				continue
			}
			if next == '*' {
				// /* block comment */ — blank, keep newlines
				j := i
				for j < n {
					if out[j] == '\n' {
						j++
						continue
					}
					if j+1 < n && out[j] == '*' && out[j+1] == '/' {
						out[j] = ' '
						out[j+1] = ' '
						j += 2
						break
					}
					out[j] = ' '
					j++
				}
				i = j
				continue
			}
		}
		i++
	}
	return out
}

// stripTrailingCommas removes ",}" and ",]" patterns introduced by
// JSON-with-trailing-commas dialects (JSONC). Mirrors stripJSONComments'
// string-literal handling.
func stripTrailingCommas(in []byte) []byte {
	out := make([]byte, 0, len(in))
	n := len(in)
	inString := false
	for i := 0; i < n; i++ {
		c := in[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < n {
				out = append(out, in[i+1])
				i++
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < n && (in[j] == ' ' || in[j] == '\t' || in[j] == '\n' || in[j] == '\r') {
				j++
			}
			if j < n && (in[j] == '}' || in[j] == ']') {
				// drop this comma
				continue
			}
		}
		out = append(out, c)
	}
	return out
}

// writeJSONObject writes obj to path as 2-space-indented JSON, with a final
// newline. The caller is responsible for backing up the file first.
func writeJSONObject(path string, obj map[string]any) error {
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(parentDir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// parentDir is a tiny helper to keep the std-lib filepath import out of
// the call sites that don't otherwise need it.
func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}

// ensureNestedMap walks/creates nested maps along keys, returning the leaf
// map. Existing values that are not maps are replaced with a fresh map —
// this is intentional, because if a user previously stored e.g.
// "mcpServers": null, the only sane thing is to overwrite with a fresh
// object. Backup makes this safe.
func ensureNestedMap(root map[string]any, keys ...string) map[string]any {
	cur := root
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	return cur
}

// equalJSON compares two values by their canonical JSON encoding. It is
// used by writers to check whether the existing block is already up to
// date and we can skip writing.
func equalJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}
