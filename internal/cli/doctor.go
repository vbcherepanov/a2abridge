package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vbcherepanov/a2abridge/internal/buildinfo"
	"github.com/vbcherepanov/a2abridge/internal/ideconfig"
)

func init() {
	registerCommand(Command{
		Name:    "doctor",
		Summary: "Health-check the directory daemon, IDE configs, skill and hooks",
		Run:     RunDoctor,
	})
}

// checkResult is one row of the doctor output.
type checkResult struct {
	Name   string
	Status string // "PASS" | "WARN" | "FAIL"
	Detail string
	Hint   string // optional fix suggestion when not PASS
}

// RunDoctor performs a battery of health checks. Returns 0 if no FAIL,
// 1 otherwise (WARN does not fail the run).
func RunDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	directoryURL := fs.String("directory", "http://127.0.0.1:7777", "directory URL to ping")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: a2abridge doctor [flags]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Runs a battery of health checks: directory daemon liveness, IDE")
		fmt.Fprintln(stderr, "configs, skill and hooks installation. Exits non-zero on FAIL.")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	results := []checkResult{}
	results = append(results, checkBinary())
	results = append(results, checkDirectoryReachable(*directoryURL))
	results = append(results, checkIDEs()...)
	results = append(results, checkSkill())
	results = append(results, checkHook())

	worst := printResults(stdout, results)

	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "platform: %s/%s, go: %s, a2abridge: %s\n",
		runtime.GOOS, runtime.GOARCH, runtime.Version(), buildinfo.Version)

	if worst == "FAIL" {
		return 1
	}
	return 0
}

func checkBinary() checkResult {
	exe, err := os.Executable()
	if err != nil {
		return checkResult{Name: "binary", Status: "WARN", Detail: "cannot resolve own path: " + err.Error()}
	}
	return checkResult{Name: "binary", Status: "PASS", Detail: exe}
}

// checkDirectoryReachable sends a 2-second GET to /agents. PASS = list
// returned. WARN = HTTP error / non-JSON / timeout — daemon may be down.
func checkDirectoryReachable(url string) checkResult {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/agents", nil)
	if err != nil {
		return checkResult{Name: "directory", Status: "FAIL", Detail: err.Error()}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{
			Name:   "directory",
			Status: "FAIL",
			Detail: fmt.Sprintf("%s unreachable (%v)", url, err),
			Hint:   "a2abridge service install && a2abridge service start",
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return checkResult{Name: "directory", Status: "FAIL", Detail: fmt.Sprintf("%s returned %d", url, resp.StatusCode)}
	}
	var peers []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return checkResult{Name: "directory", Status: "WARN", Detail: "invalid JSON response: " + err.Error()}
	}
	return checkResult{
		Name:   "directory",
		Status: "PASS",
		Detail: fmt.Sprintf("%s reachable, %d peer(s) registered", url, len(peers)),
	}
}

// checkIDEs runs each ideconfig writer in dry-run mode and reports
// whether the a2abridge MCP block is currently registered.
func checkIDEs() []checkResult {
	out := make([]checkResult, 0, 6)
	spec := ideconfig.DefaultSpec(currentExe())
	for _, w := range ideconfig.AllWriters() {
		path := w.Detect()
		if path == "" {
			out = append(out, checkResult{
				Name:   "ide:" + w.Name(),
				Status: "WARN",
				Detail: "not detected on this machine",
			})
			continue
		}
		res := w.Write(spec, true) // dry-run
		switch {
		case res.Error != nil && strings.Contains(res.Error.Error(), "VS Code not detected"):
			out = append(out, checkResult{
				Name:   "ide:" + w.Name(),
				Status: "WARN",
				Detail: res.Error.Error(),
			})
		case res.Error != nil:
			out = append(out, checkResult{
				Name:   "ide:" + w.Name(),
				Status: "FAIL",
				Detail: res.Error.Error(),
				Hint:   "a2abridge install --apply --ide " + writerSlug(w),
			})
		case res.Skipped:
			out = append(out, checkResult{
				Name:   "ide:" + w.Name(),
				Status: "PASS",
				Detail: "MCP block present at " + path,
			})
		default:
			out = append(out, checkResult{
				Name:   "ide:" + w.Name(),
				Status: "WARN",
				Detail: "config exists at " + path + " but a2a MCP block is missing or stale",
				Hint:   "a2abridge install --apply --ide " + writerSlug(w),
			})
		}
	}
	return out
}

func checkSkill() checkResult {
	h, err := homeDirOnce()
	if err != nil {
		return checkResult{Name: "skill", Status: "WARN", Detail: err.Error()}
	}
	skill := filepath.Join(h, ".claude", "skills", "a2a-bridge", "SKILL.md")
	if !fileExistsAt(skill) {
		return checkResult{
			Name:   "skill",
			Status: "WARN",
			Detail: "skill not installed at " + skill,
			Hint:   "a2abridge install --apply  (drops the skill alongside MCP configs)",
		}
	}
	return checkResult{Name: "skill", Status: "PASS", Detail: skill}
}

func checkHook() checkResult {
	h, err := homeDirOnce()
	if err != nil {
		return checkResult{Name: "hook", Status: "WARN", Detail: err.Error()}
	}
	hook := filepath.Join(h, ".claude", "hooks", "a2a-inbox-hook.sh")
	if !fileExistsAt(hook) {
		return checkResult{
			Name:   "hook",
			Status: "WARN",
			Detail: "UserPromptSubmit hook not installed at " + hook,
			Hint:   "a2abridge install --apply  (writes the hook)",
		}
	}
	info, _ := os.Stat(hook)
	if info != nil && info.Mode().Perm()&0o111 == 0 {
		return checkResult{
			Name:   "hook",
			Status: "WARN",
			Detail: hook + " is present but not executable",
			Hint:   "chmod +x " + hook,
		}
	}
	return checkResult{Name: "hook", Status: "PASS", Detail: hook}
}

func printResults(w io.Writer, results []checkResult) string {
	worst := "PASS"
	for _, r := range results {
		switch r.Status {
		case "FAIL":
			worst = "FAIL"
		case "WARN":
			if worst != "FAIL" {
				worst = "WARN"
			}
		}
		fmt.Fprintf(w, "  [%s] %-22s %s\n", r.Status, r.Name, r.Detail)
		if r.Hint != "" {
			fmt.Fprintf(w, "         %-22s   ↳ fix: %s\n", "", r.Hint)
		}
	}
	return worst
}

func currentExe() string {
	if e, err := os.Executable(); err == nil {
		return e
	}
	return "a2abridge"
}

func homeDirOnce() (string, error) {
	return os.UserHomeDir()
}

func fileExistsAt(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
