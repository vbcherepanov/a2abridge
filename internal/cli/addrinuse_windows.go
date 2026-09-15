//go:build windows

package cli

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isAddrInUse reports whether a listen error means another process holds the
// port. Winsock returns WSAEADDRINUSE, not the POSIX EADDRINUSE value.
func isAddrInUse(err error) bool {
	return errors.Is(err, windows.WSAEADDRINUSE) || errors.Is(err, syscall.EADDRINUSE)
}
