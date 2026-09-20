//go:build windows

package svcd

import (
	"errors"

	"golang.org/x/sys/windows"
)

// connRefused reports whether a dial failed because the address was reached
// and nothing listened there — the ONE outcome that proves a port is free.
//
// On Windows the socket layer returns WSAECONNREFUSED (10061), and Go's
// syscall.ECONNREFUSED is a synthetic APPLICATION_ERROR value that never
// equals it, so `errors.Is(err, syscall.ECONNREFUSED)` is false for a real
// refusal here. With that check, every status/start/stop on a Windows host
// read a free port as "in use by an unidentified listener" and the console
// could never be started under the supervisor.
func connRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
