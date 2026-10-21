//go:build !linux

package procfs

import (
	"errors"
	"os"
)

var errUnsupported = errors.New("procfs: only supported on linux")

// Handle is a procfs root handle; procfs exists only on Linux.
type Handle struct{}

// OpenProcRoot is unsupported off Linux.
func OpenProcRoot() (*Handle, error) { return nil, errUnsupported }

// OpenUnsafeProcRoot is unsupported off Linux.
func OpenUnsafeProcRoot() (*Handle, error) { return nil, errUnsupported }

// Close is a no-op off Linux.
func (h *Handle) Close() error { return nil }

// OpenSelf is unsupported off Linux.
func (h *Handle) OpenSelf(string) (*os.File, error) { return nil, errUnsupported }

// OpenPid is unsupported off Linux.
func (h *Handle) OpenPid(int, string) (*os.File, error) { return nil, errUnsupported }

// OpenThreadSelf is unsupported off Linux.
func (h *Handle) OpenThreadSelf(string) (*os.File, ProcThreadSelfCloser, error) {
	return nil, nil, errUnsupported
}
