//go:build !unix && !windows

package svcd

import (
	"errors"
	"syscall"
)

func connRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
