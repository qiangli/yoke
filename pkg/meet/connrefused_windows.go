//go:build windows

package meet

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isConnRefused reports whether a dial error is an explicit refusal — the
// address was reached and nothing listened. Winsock reports that as
// WSAECONNREFUSED (10061); syscall.ECONNREFUSED is an errno Go INVENTS on
// Windows (APPLICATION_ERROR+n) that the network stack never returns, so
// matching only it read every free port as an unidentified listener.
func isConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED)
}
