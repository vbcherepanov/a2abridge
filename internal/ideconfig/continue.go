package ideconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// continueWriter handles Continue's MCP server registration.
//
// Continue 1.x uses YAML config; instead of mutating the user's main
// config.yaml (which often has anchors/comments) we drop a dedicated file
// at ~/.continue/mcpServers/a2a.yaml — which Continue automatically picks
// up.
//
// Schema (from Continue docs):
//
//	name: a2a
//	version: 0.0.1
//	schema: v1
//	mcpServers:
//	  - name: a2a
//	    command: /path/to/a2abridge
//	    args:
//	      - bridge
//	    env:
//	      A2A_DIRECTORY: http://127.0.0.1:7777
type continueWriter struct{}

func (continueWriter) Name() string { return "Continue" }

func (continueWriter) Detect() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	// Auto-detect uses ~/.continue (the IDE's marker dir). The actual file
	// we write is ~/.continue/mcpServers/a2a.yaml (returned by writeTarget).
	// Without this split, auto-mode would always think Continue is missing
	// because our own a2a.yaml never exists before first apply.
	root := filepath.Join(h, ".continue")
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return root
	}
	return ""
}

// writeTarget returns the actual file we will write to (a sub-path of the
// detected marker directory). Used internally — Detect must continue to
// answer the "is this IDE installed?" question.
func (continueWriter) writeTarget() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".continue", "mcpServers", "a2a.yaml")
}

func (w continueWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.writeTarget()
	if path == "" {
		res.Error = fmt.Errorf("could not resolve Continue config path")
		return res
	}
	res.Path = path
	res.Found = fileExists(path)

	desired := continueYAML(spec)
	// Cheap idempotency: if the file already exists with byte-identical
	// content, skip. We don't full-parse the YAML because there's nothing
	// for the user to round-trip — this file is wholly owned by us.
	if res.Found {
		existing, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(existing)) == strings.TrimSpace(desired) {
			res.Skipped = true
			return res
		}
	}

	if dryRun {
		res.Updated = true
		return res
	}

	if res.Found {
		bak, berr := backupFile(path)
		if berr != nil {
			res.Error = fmt.Errorf("backup: %w", berr)
			return res
		}
		res.Backup = bak
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		res.Error = err
		return res
	}
	if err := os.WriteFile(path, []byte(desired), 0o644); err != nil {
		res.Error = err
		return res
	}
	res.Updated = true
	return res
}

// continueYAML renders the MCP server file. We hand-write the YAML to
// avoid a yaml dependency for one tiny file.
func continueYAML(spec Spec) string {
	var b strings.Builder
	b.WriteString("name: a2a\n")
	b.WriteString("version: 0.0.1\n")
	b.WriteString("schema: v1\n")
	b.WriteString("mcpServers:\n")
	b.WriteString("  - name: a2a\n")
	fmt.Fprintf(&b, "    command: %s\n", yamlScalar(spec.BinaryPath))
	b.WriteString("    args:\n")
	b.WriteString("      - bridge\n")
	if len(spec.Env) > 0 {
		b.WriteString("    env:\n")
		for _, k := range sortedKeys(spec.Env) {
			fmt.Fprintf(&b, "      %s: %s\n", k, yamlScalar(spec.Env[k]))
		}
	}
	return b.String()
}

// yamlScalar quotes a string only when it contains characters that would
// otherwise be interpreted as YAML structure. Keeping unquoted output for
// simple paths and URLs makes diffs cleaner.
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	for _, c := range s {
		if c == ':' || c == '#' || c == '"' || c == '\'' || c == '\n' {
			return strings.ReplaceAll(`"`+s+`"`, "\n", "\\n")
		}
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// std sort.Strings — deterministic order in YAML output for reproducible diffs.
	sortStrings(out)
	return out
}

// Local sort to avoid pulling sort just for this in tests; tiny insertion sort.
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
