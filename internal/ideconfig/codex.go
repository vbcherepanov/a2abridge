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
//
// The file is patched in place (see codextoml.go), so the user's comments,
// key order and every table this package does not manage stay as they were.
type codexWriter struct{}

func (codexWriter) Name() string { return "Codex CLI" }

// Detect answers "is Codex CLI installed?". Codex creates ~/.codex on first
// run (auth.json, history) but config.toml only once the user adds a setting,
// so the state directory also counts as a marker.
func (w codexWriter) Detect() string {
	if target := w.writeTarget(); fileExists(target) {
		return target
	}
	h, err := homeDir()
	if err != nil {
		return ""
	}
	if marker := filepath.Join(h, ".codex"); dirExists(marker) {
		return marker
	}
	return ""
}

// writeTarget is the global config Codex CLI reads MCP servers from.
func (codexWriter) writeTarget() string {
	h, err := homeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".codex", "config.toml")
}

func (w codexWriter) Write(spec Spec, dryRun bool) Result {
	res := Result{IDE: w.Name(), DryRun: dryRun}
	path := w.writeTarget()
	if path == "" {
		res.Error = fmt.Errorf("could not resolve Codex config path")
		return res
	}
	res.Path = path
	res.Found = fileExists(path)

	raw, root, err := readTOMLConfig(path)
	if err != nil {
		res.Error = err
		return res
	}

	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		root["mcp_servers"] = servers
	}

	existing := servers[spec.Key]
	desired := mergeMCPEntry(existing, spec)
	if equalTOMLEntry(existing, desired) {
		res.Skipped = true
		return res
	}
	servers[spec.Key] = desired

	if dryRun {
		res.Updated = true
		return res
	}

	out, inPlace, err := renderTOMLConfig(root, func() ([]byte, error) {
		return patchCodexEntry(raw, spec.Key, existing, desired)
	})
	if err != nil {
		res.Error = err
		return res
	}
	if !inPlace && res.Found {
		res.Note = rewriteNote
	}

	if res.Found {
		bak, berr := backupFile(path)
		if berr != nil {
			res.Error = fmt.Errorf("backup: %w", berr)
			return res
		}
		res.Backup = bak
	}
	if err := writeFileAtomic(path, out, newConfigMode); err != nil {
		res.Error = err
		return res
	}
	res.Updated = true
	return res
}

// rewriteNote explains the fallback path of renderTOMLConfig to the user.
const rewriteNote = "config layout could not be patched in place; file was re-encoded, comments are in the backup"

// readTOMLConfig returns the raw bytes of path and its decoded top-level
// table. A missing or empty file yields an empty table.
func readTOMLConfig(path string) (raw []byte, root map[string]any, err error) {
	root = map[string]any{}
	raw, err = os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, root, nil
		}
		return nil, nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, root, nil
	}
	if err := toml.Unmarshal(raw, &root); err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return raw, root, nil
}

// renderTOMLConfig returns the bytes to write for want. It prefers the
// result of patch, a text edit of the original file, and accepts it only
// when it decodes to exactly want. Otherwise it re-encodes want from
// scratch, which is always correct but drops comments and key order.
// inPlace reports which of the two was used.
func renderTOMLConfig(want map[string]any, patch func() ([]byte, error)) (out []byte, inPlace bool, err error) {
	if patched, perr := patch(); perr == nil {
		got := map[string]any{}
		if uerr := toml.Unmarshal(patched, &got); uerr == nil && equalTOMLEntry(dropEmptyServers(got), dropEmptyServers(want)) {
			return patched, true, nil
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(want); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), false, nil
}

// dropEmptyServers returns root without an empty mcp_servers table, so a
// leftover "[mcp_servers]" header compares equal to no header at all.
func dropEmptyServers(root map[string]any) map[string]any {
	servers, ok := root["mcp_servers"].(map[string]any)
	if !ok || len(servers) > 0 {
		return root
	}
	out := make(map[string]any, len(root))
	for k, v := range root {
		if k != "mcp_servers" {
			out[k] = v
		}
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
