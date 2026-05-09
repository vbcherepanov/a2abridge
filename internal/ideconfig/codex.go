package ideconfig

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// codexWriter handles Codex CLI's TOML config:
//
//	[mcp_servers.a2a]
//	command = "/path/to/a2abridge"
//	args    = ["bridge"]
//
//	[mcp_servers.a2a.env]
//	A2A_DIRECTORY = "..."
type codexWriter struct{}

func (codexWriter) Name() string { return "Codex CLI" }

func (codexWriter) Detect() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".codex", "config.toml")
}

func (w codexWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.Detect()
	if path == "" {
		res.Error = fmt.Errorf("could not resolve Codex config path")
		return res
	}
	res.Path = path
	res.Found = fileExists(path)

	// BurntSushi/toml decodes into a generic map so we can keep all the
	// user's other keys intact.
	root := map[string]any{}
	if res.Found {
		b, err := os.ReadFile(path)
		if err != nil {
			res.Error = err
			return res
		}
		if len(b) > 0 {
			if err := toml.Unmarshal(b, &root); err != nil {
				res.Error = fmt.Errorf("parse %s: %w", path, err)
				return res
			}
		}
	}

	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		root["mcp_servers"] = servers
	}

	desired := codexEntry(spec)
	if equalTOMLEntry(servers[spec.Key], desired) {
		res.Skipped = true
		return res
	}
	servers[spec.Key] = desired

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

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(root); err != nil {
		res.Error = err
		return res
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		res.Error = err
		return res
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		res.Error = err
		return res
	}
	res.Updated = true
	return res
}

// codexEntry produces the table value Codex expects.
func codexEntry(spec Spec) map[string]any {
	out := map[string]any{
		"command": spec.BinaryPath,
		"args":    []any{"bridge"},
	}
	if len(spec.Env) > 0 {
		envTable := map[string]any{}
		for k, v := range spec.Env {
			envTable[k] = v
		}
		out["env"] = envTable
	}
	return out
}

// equalTOMLEntry compares two table values by re-encoding to TOML. Cheap
// and keeps us from caring about map[string]any vs map[string]string subtleties.
func equalTOMLEntry(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	var ab, bb bytes.Buffer
	if err := toml.NewEncoder(&ab).Encode(a); err != nil {
		return false
	}
	if err := toml.NewEncoder(&bb).Encode(b); err != nil {
		return false
	}
	return ab.String() == bb.String()
}
