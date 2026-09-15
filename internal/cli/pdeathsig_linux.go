//go:build linux

package cli

import (
	"log/slog"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// setParentDeathSignal asks the kernel to send SIGTERM to this process when its
// parent (the MCP host, e.g. claude) dies. That routes into the
// signal.NotifyContext(SIGTERM) handler in RunBridge, triggering graceful
// shutdown so the HTTP listener is closed and the port released.
//
// Without it, a bridge whose parent exits reparents to init (ppid 1) and keeps
// holding its port; the next bridge then cannot bind ("address already in use")
// and the agent's outbound send path goes silently dead until the orphan is
// reaped.
//
// PR_SET_PDEATHSIG is a per-THREAD attribute that is cleared if the thread that
// set it exits, and the Go runtime freely migrates goroutines across — and
// retires — OS threads. So we set it on a dedicated thread that is locked and
// then blocks forever, guaranteeing the attribute outlives startup.
func setParentDeathSignal(log *slog.Logger) {
	go func() {
		// Never unlocked on the success path: keep this OS thread (and the
		// pdeathsig setting attached to it) alive for the life of the process.
		runtime.LockOSThread()

		if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0, 0, 0); err != nil {
			log.Warn("pdeathsig: prctl failed; relying on stdin-EOF shutdown", "err", err)
			runtime.UnlockOSThread()
			return
		}

		// Race: the parent may have already exited between fork and now, in which
		// case the death signal will never arrive. Detect the orphan and shut
		// down immediately by signalling ourselves (routed to graceful shutdown).
		if os.Getppid() == 1 {
			log.Info("pdeathsig: parent already gone at startup, shutting down")
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			runtime.UnlockOSThread()
			return
		}

		select {} // hold the locked thread forever so the pdeathsig setting persists
	}()
}
