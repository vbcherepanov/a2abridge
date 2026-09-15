//go:build !windows

package cli

import (
	"errors"
	"syscall"
)

// isAddrInUse reports whether a listen error means another process holds the
// port.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
