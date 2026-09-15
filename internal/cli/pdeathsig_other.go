//go:build !linux

package cli

import "log/slog"

// setParentDeathSignal is a no-op on non-Linux platforms, which have no
// equivalent of PR_SET_PDEATHSIG. Parent-death handling there relies on the MCP
// stdio server returning on stdin EOF (see RunBridge), which fires when the
// parent closes the pipe on exit.
func setParentDeathSignal(_ *slog.Logger) {}
