//go:build windows

package audacity

import (
	"context"
	"errors"
)

// On Windows the Audacity mod-script-pipe uses named pipes
// (\\.\pipe\ToSrvPipe / FromSrvPipe). This story does NOT implement the
// Windows named-pipe transport: defaultPipeClient fails closed on Windows
// before reaching the pipe layer (see EVIDENCE.md). These stubs exist only
// so the package cross-compiles and cross-vets on windows/amd64; they are
// never reached in production on this platform.

var errWindowsProvisionOnly = errors.New("audacity: pipe transport not implemented on Windows (provision-only; use Linux)")

func verifyPipe(path string, wantUid int) error { return errWindowsProvisionOnly }

func mkFifo(path string, mode uint32) error {
	return errors.New("audacity: mkFifo not implemented on Windows")
}

// fifo is the unreachable Windows stand-in for the Unix FIFO end.
type fifo struct{}

func openFIFOForWrite(ctx context.Context, path string) (*fifo, error) {
	return nil, errWindowsProvisionOnly
}

func openFIFOForRead(ctx context.Context, path string) (*fifo, error) {
	return nil, errWindowsProvisionOnly
}

func (f *fifo) Close() error                                 { return nil }
func (f *fifo) writeAll(ctx context.Context, p []byte) error { return errWindowsProvisionOnly }
func (f *fifo) readSome(ctx context.Context, b []byte) (int, error) {
	return 0, errWindowsProvisionOnly
}
