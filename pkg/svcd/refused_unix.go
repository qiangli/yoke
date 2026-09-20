//go:build unix

package svcd

import (
	"errors"
	"syscall"
)

// connRefused reports whether a dial failed because the address was reached
// and nothing listened there — the ONE outcome that proves a port is free.
func connRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
