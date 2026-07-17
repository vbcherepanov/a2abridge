//go:build integration

// Integration tests for the "bridge dies with its parent" + "one bridge per
// agent" fix. They build and run the real a2abridge binary, so they are gated
// behind the `integration` build tag and are NOT part of the default
// `go test ./...` / CI unit run:
//
//	go test -tags=integration ./internal/cli/ -run TestBridge -v
//
// They assert observable behavior (exit code, port release, process death), so
// they exercise the shipping code as-is and port unchanged to any base
// (the fleet v3.0.3 blob and the upstream a2abridge#20 branch alike).
// TestBridgeDiesOnParentDeath additionally requires Linux (PR_SET_PDEATHSIG) and
// skips elsewhere.
package cli_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var bridgeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "a2abridge-itest-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	bridgeBin = filepath.Join(dir, "a2abridge")
	build := exec.Command("go", "build", "-o", bridgeBin, "./cmd/a2abridge")
	build.Dir = "../.." // module root, relative to internal/cli
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build a2abridge:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// bridgeArgs returns the flags for a quiet, self-contained bridge on bindAddr
// with its own state dir (directory points nowhere so heartbeat just fails
// harmlessly). Pass "127.0.0.1:0" for a random port, or a fixed addr to make
// two bridges contend for the same port (the singleton lock).
func bridgeArgs(bindAddr, stateDir, id string) []string {
	return []string{
		"bridge",
		"--bind", bindAddr,
		"--state-dir", stateDir,
		"--name", "itest",
		"--id", id,
		"--directory", "http://127.0.0.1:1",
	}
}

var listenRe = regexp.MustCompile(`"url":"https?://127\.0\.0\.1:(\d+)"`)

// waitForPort polls the bridge's own log for the "a2a server listening" line and
// returns the port it actually bound.
func waitForPort(t *testing.T, stateDir string) int {
	t.Helper()
	logPath := filepath.Join(stateDir, "bridge.log")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(logPath); err == nil {
			if m := listenRe.FindSubmatch(b); m != nil {
				p, _ := strconv.Atoi(string(m[1]))
				return p
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("bridge never logged a listening port (state dir %s)", stateDir)
	return 0
}

// freePort returns a currently-free localhost TCP port. There is an inherent
// TOCTOU gap, but the caller binds it immediately and the tests target the
// loopback in isolation.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// portFree reports whether 127.0.0.1:port can be bound (i.e. released).
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitUntil polls cond until it is true or the timeout elapses.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// childOfViaProc returns the first child process of ppid per /proc (Linux).
func childOfViaProc(ppid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		// /proc/<pid>/stat: "pid (comm) state ppid ...". comm can contain spaces
		// and parens, so parse after the final ')': fields are state, ppid, ...
		s := string(stat)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) >= 2 {
			if pp, _ := strconv.Atoi(fields[1]); pp == ppid {
				return pid
			}
		}
	}
	return 0
}

// TestBridgeSecondInstanceDefersToIncumbent: two bridges targeting the SAME
// bind address model a fast parent-restart / duplicate launch. The incumbent
// keeps the port; the duplicate must retry, then defer and exit CLEANLY (0)
// WITHOUT killing the incumbent and WITHOUT running portless — exactly one
// bridge per agent.
func TestBridgeSecondInstanceDefersToIncumbent(t *testing.T) {
	addr := fmt.Sprintf("127.0.0.1:%d", freePort(t))

	// Incumbent: hold its stdin open so it keeps running for the whole test.
	sdA := t.TempDir()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inW.Close() }()
	incumbent := exec.Command(bridgeBin, bridgeArgs(addr, sdA, "incumbent")...)
	incumbent.Stdin = inR
	if err := incumbent.Start(); err != nil {
		t.Fatal(err)
	}
	_ = inR.Close()
	t.Cleanup(func() { _ = incumbent.Process.Kill() })

	port := waitForPort(t, sdA)
	if portFree(port) {
		t.Fatalf("incumbent should be holding port %d", port)
	}

	// Duplicate: same addr. It should retry (~10x300ms) then defer + exit 0.
	sdB := t.TempDir()
	dup := exec.Command(bridgeBin, bridgeArgs(addr, sdB, "duplicate")...)
	dup.Stdin, _ = os.Open(os.DevNull) // duplicate's stdin is irrelevant to the defer path
	if err := dup.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- dup.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("duplicate should defer and exit 0, got: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = dup.Process.Kill()
		t.Fatal("duplicate did not exit within 10s (should defer after ~3s)")
	}

	// The incumbent must be untouched: still alive, still holding the port.
	if !pidAlive(incumbent.Process.Pid) {
		t.Fatal("incumbent was killed — a duplicate must never sacrifice the incumbent")
	}
	if portFree(port) {
		t.Fatalf("port %d not held by the incumbent after the duplicate deferred", port)
	}
}

// TestBridgeShutsDownOnStdinEOF: when the MCP host closes the bridge's stdin
// (the host exiting closes the pipe), ServeStdio returns and the bridge shuts
// down and frees its port — even on a clean EOF.
func TestBridgeShutsDownOnStdinEOF(t *testing.T) {
	sd := t.TempDir()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bridgeBin, bridgeArgs("127.0.0.1:0", sd, "eof")...)
	cmd.Stdin = stdinR
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdinR.Close() // the child holds its own dup; we keep stdinW
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	port := waitForPort(t, sd)
	if portFree(port) {
		t.Fatalf("bridge should be holding port %d while running", port)
	}

	// Close stdin → EOF → ServeStdio returns → shutdown.
	_ = stdinW.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge exited non-zero on stdin EOF: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("bridge did not exit within 8s of stdin EOF")
	}

	if !waitUntil(3*time.Second, func() bool { return portFree(port) }) {
		t.Fatalf("port %d not released after stdin-EOF shutdown", port)
	}
}

// TestBridgeDiesOnParentDeath (Linux only): when the bridge's parent dies, the
// PR_SET_PDEATHSIG(SIGTERM) arm shuts it down and frees the port — even though
// its stdin is STILL OPEN (this test holds the pipe's write end for the whole
// run), which isolates the death to pdeathsig rather than the stdin-EOF path.
func TestBridgeDiesOnParentDeath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("PR_SET_PDEATHSIG is Linux-only; GOOS=%s", runtime.GOOS)
	}

	sd := t.TempDir()

	// stdinR feeds the sh parent, which the bridge inherits as its own stdin.
	// The test keeps stdinW open for the whole run so the bridge's stdin never
	// EOFs — the only thing that can kill it is the parent-death signal.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdinW.Close() }()

	// Run the bridge in the FOREGROUND under sh so it inherits sh's (open)
	// stdin — a backgrounded (`&`) job would have its stdin redirected to
	// /dev/null by the shell and EOF immediately, hiding the pdeathsig path.
	// The trailing "; true" stops sh from exec-replacing itself with the bridge,
	// so sh stays alive as the killable parent.
	script := fmt.Sprintf("%s %s; true", bridgeBin, strings.Join(bridgeArgs("127.0.0.1:0", sd, "pdeath"), " "))
	parent := exec.Command("sh", "-c", script)
	parent.Stdin = stdinR
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	_ = stdinR.Close() // the child holds its own dup; the test keeps stdinW
	reaped := make(chan struct{})
	go func() { _ = parent.Wait(); close(reaped) }() // reap the sh zombie after we kill it
	defer func() { _ = parent.Process.Kill() }()

	port := waitForPort(t, sd)

	// The bridge is sh's sole child; find it and confirm it's live with stdin
	// still open (no premature EOF).
	var bridgePid int
	if !waitUntil(3*time.Second, func() bool {
		bridgePid = childOfViaProc(parent.Process.Pid)
		return bridgePid > 0
	}) {
		t.Fatal("never found the bridge child of the sh parent")
	}
	if !pidAlive(bridgePid) {
		if b, err := os.ReadFile(filepath.Join(sd, "bridge.log")); err == nil {
			t.Logf("bridge.log:\n%s", b)
		}
		t.Fatalf("bridge child %d not alive before the parent is killed (premature stdin EOF?)", bridgePid)
	}

	// Kill ONLY the parent. The bridge must die via pdeathsig (stdin still open).
	if err := parent.Process.Kill(); err != nil {
		t.Fatalf("killing parent: %v", err)
	}
	<-reaped

	if !waitUntil(8*time.Second, func() bool { return !pidAlive(bridgePid) }) {
		t.Fatalf("bridge child %d survived its parent's death (pdeathsig did not fire)", bridgePid)
	}
	if !waitUntil(3*time.Second, func() bool { return portFree(port) }) {
		t.Fatalf("port %d not released after parent-death shutdown", port)
	}
}
