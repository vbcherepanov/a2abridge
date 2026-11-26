package ideconfig

import (
	"bytes"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// This file edits Codex's config.toml as text. Decoding the file and
// encoding it back would be shorter, but it drops every comment and
// reorders every table, and people keep both in that file. The edits here
// touch only the lines of [mcp_servers.<key>] and its env table. Callers
// check the result by decoding it (renderTOMLConfig), so a layout these
// line-based edits do not understand falls back to a full re-encode
// instead of producing a wrong file.

const (
	// codexServerKey is the name of our entry under [mcp_servers].
	codexServerKey = "a2a"

	codexServersTable = "mcp_servers"
	codexEnvTable     = "env"
	tomlLF            = "\n"
	tomlCRLF          = "\r\n"
)

var (
	// tomlHeaderRe matches "[a.b]" and "[[a.b]]" header lines.
	tomlHeaderRe = regexp.MustCompile(`^\s*\[\[?\s*([^\[\]]+?)\s*\]\]?\s*(#.*)?$`)
	// tomlBareKeyRe matches keys that need no quoting.
	tomlBareKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// tomlInlineEnvRe matches an env inline table inside the server table.
	tomlInlineEnvRe = regexp.MustCompile(`^(\s*)env\s*=\s*\{`)
)

// tomlTable is one table header and the lines it owns.
type tomlTable struct {
	path  []string
	start int // index of the header line
	end   int // index of the first line after the table
}

// tomlDoc is a TOML file split into lines.
type tomlDoc struct {
	lines []string
	eol   string
}

func newTOMLDoc(src []byte) *tomlDoc {
	doc := &tomlDoc{eol: tomlLF}
	text := string(src)
	if strings.Contains(text, tomlCRLF) {
		doc.eol = tomlCRLF
		text = strings.ReplaceAll(text, tomlCRLF, tomlLF)
	}
	text = strings.TrimRight(text, tomlLF)
	if text != "" {
		doc.lines = strings.Split(text, tomlLF)
	}
	return doc
}

func (d *tomlDoc) bytes() []byte {
	if len(d.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(d.lines, d.eol) + d.eol)
}

// tables lists every table header in document order.
func (d *tomlDoc) tables() []tomlTable {
	var out []tomlTable
	for i, line := range d.lines {
		m := tomlHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if n := len(out); n > 0 {
			out[n-1].end = i
		}
		out = append(out, tomlTable{path: splitTOMLKey(m[1]), start: i, end: len(d.lines)})
	}
	return out
}

// find returns the table with exactly this path.
func (d *tomlDoc) find(path []string) (tomlTable, bool) {
	for _, t := range d.tables() {
		if reflect.DeepEqual(t.path, path) {
			return t, true
		}
	}
	return tomlTable{}, false
}

func (d *tomlDoc) insert(at int, lines ...string) {
	out := make([]string, 0, len(d.lines)+len(lines))
	out = append(out, d.lines[:at]...)
	out = append(out, lines...)
	out = append(out, d.lines[at:]...)
	d.lines = out
}

func (d *tomlDoc) remove(from, to int) {
	d.lines = append(d.lines[:from], d.lines[to:]...)
}

// contentEnd returns the index just past the last key/value line of t.
// Blank lines and comments after it are left to the table that follows,
// because a comment above a header describes that header.
func (d *tomlDoc) contentEnd(t tomlTable) int {
	for i := t.end - 1; i > t.start; i-- {
		trimmed := strings.TrimSpace(d.lines[i])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return i + 1
		}
	}
	return t.start + 1
}

// setKey replaces the "key = ..." line of the table at path, or adds one
// after the table's last key.
func (d *tomlDoc) setKey(path []string, key, value string) error {
	t, ok := d.find(path)
	if !ok {
		return fmt.Errorf("table %s not found", strings.Join(path, "."))
	}
	bare := regexp.QuoteMeta(key)
	keyRe, err := regexp.Compile(`^(\s*)(?:` + bare + `|"` + bare + `"|'` + bare + `')\s*=`)
	if err != nil {
		return err
	}
	for i := t.start + 1; i < t.end; i++ {
		if m := keyRe.FindStringSubmatch(d.lines[i]); m != nil {
			d.lines[i] = m[1] + tomlKey(key) + " = " + value
			return nil
		}
	}
	d.insert(d.contentEnd(t), tomlKey(key)+" = "+value)
	return nil
}

// patchCodexEntry rewrites src so that mcp_servers.<key> decodes to desired.
// existing is what the entry decodes to now (nil when absent); only the
// keys whose value differs are touched.
func patchCodexEntry(src []byte, key string, existing any, desired map[string]any) ([]byte, error) {
	doc := newTOMLDoc(src)
	current, _ := existing.(map[string]any)
	serverPath := []string{codexServersTable, key}

	if _, ok := doc.find(serverPath); !ok {
		doc.addServerTable(serverPath)
		current = nil
	}
	for _, k := range []string{"command", "args"} {
		if current != nil && reflect.DeepEqual(current[k], desired[k]) {
			continue
		}
		value, err := tomlValue(desired[k])
		if err != nil {
			return nil, err
		}
		if err := doc.setKey(serverPath, k, value); err != nil {
			return nil, err
		}
	}

	env, _ := desired[codexEnvTable].(map[string]any)
	currentEnv, _ := current[codexEnvTable].(map[string]any)
	if err := doc.patchEnv(serverPath, currentEnv, env); err != nil {
		return nil, err
	}
	return doc.bytes(), nil
}

// addServerTable inserts an empty server table: ahead of its sub-tables
// when some already exist, otherwise at the end of the file.
func (d *tomlDoc) addServerTable(serverPath []string) {
	header := "[" + tomlKeyPath(serverPath) + "]"
	for _, t := range d.tables() {
		if hasPathPrefix(t.path, serverPath) {
			d.insert(t.start, header, "")
			return
		}
	}
	if len(d.lines) > 0 {
		d.lines = append(d.lines, "")
	}
	d.lines = append(d.lines, header)
}

// patchEnv brings the env of the server table in line with want. It edits
// the [..env] table when there is one, an inline "env = {...}" when the
// user wrote it that way, and otherwise adds the table.
func (d *tomlDoc) patchEnv(serverPath []string, current, want map[string]any) error {
	changed := make([]string, 0, len(want))
	for k, v := range want {
		if cur, has := current[k]; !has || !reflect.DeepEqual(cur, v) {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)

	envPath := append(append([]string{}, serverPath...), codexEnvTable)
	if _, ok := d.find(envPath); ok {
		for _, k := range changed {
			value, err := tomlValue(want[k])
			if err != nil {
				return err
			}
			if err := d.setKey(envPath, k, value); err != nil {
				return err
			}
		}
		return nil
	}

	server, ok := d.find(serverPath)
	if !ok {
		return fmt.Errorf("table %s not found", strings.Join(serverPath, "."))
	}
	pairs, err := tomlPairs(want)
	if err != nil {
		return err
	}
	for i := server.start + 1; i < server.end; i++ {
		if m := tomlInlineEnvRe.FindStringSubmatch(d.lines[i]); m != nil {
			d.lines[i] = m[1] + "env = { " + strings.Join(pairs, ", ") + " }"
			return nil
		}
	}

	at := d.contentEnd(server)
	block := append([]string{"", "[" + tomlKeyPath(envPath) + "]"}, pairs...)
	if at < len(d.lines) && strings.TrimSpace(d.lines[at]) != "" {
		block = append(block, "")
	}
	d.insert(at, block...)
	return nil
}

// stripCodexEntry removes [mcp_servers.<key>] and every table below it.
func stripCodexEntry(src []byte, key string) []byte {
	doc := newTOMLDoc(src)
	serverPath := []string{codexServersTable, key}
	for {
		removed := false
		for _, t := range doc.tables() {
			if !hasPathPrefix(t.path, serverPath) {
				continue
			}
			doc.remove(t.start, doc.contentEnd(t))
			doc.squeezeBlank(t.start)
			removed = true
			break
		}
		if !removed {
			break
		}
	}
	return doc.bytes()
}

// squeezeBlank drops the blank lines a removal left doubled up around at,
// and any blank lines now trailing the file.
func (d *tomlDoc) squeezeBlank(at int) {
	blank := func(i int) bool { return strings.TrimSpace(d.lines[i]) == "" }
	for at < len(d.lines) && blank(at) && (at == 0 || blank(at-1)) {
		d.remove(at, at+1)
	}
	for n := len(d.lines); n > 0 && blank(n-1); n = len(d.lines) {
		d.remove(n-1, n)
	}
}

// hasPathPrefix reports whether path equals prefix or lies below it.
func hasPathPrefix(path, prefix []string) bool {
	return len(path) >= len(prefix) && reflect.DeepEqual(path[:len(prefix)], prefix)
}

// splitTOMLKey splits a dotted key such as mcp_servers."a2a".env into its
// parts, dropping the quotes.
func splitTOMLKey(s string) []string {
	var (
		parts []string
		cur   strings.Builder
		quote byte
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '.':
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(parts, strings.TrimSpace(cur.String()))
}

// tomlKey renders one key, quoting it when TOML requires that.
func tomlKey(k string) string {
	if tomlBareKeyRe.MatchString(k) {
		return k
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(k) + `"`
}

func tomlKeyPath(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = tomlKey(p)
	}
	return strings.Join(parts, ".")
}

// tomlValue renders v the way the TOML encoder would on the right-hand
// side of "key = ".
func tomlValue(v any) (string, error) {
	const probe = "v"
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(map[string]any{probe: v}); err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(buf.String()), probe+" =")), nil
}

// tomlPairs renders m as sorted "key = value" strings.
func tomlPairs(m map[string]any) ([]string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		value, err := tomlValue(m[k])
		if err != nil {
			return nil, err
		}
		out = append(out, tomlKey(k)+" = "+value)
	}
	return out, nil
}
