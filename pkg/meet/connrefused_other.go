//go:build !windows

package meet

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether a dial error is an explicit refusal — the
// address was reached and nothing listened. See connrefused_windows.go for why
// this is per-platform.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
