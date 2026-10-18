//go:build !windows

package audacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// verifyPipe checks that path is a FIFO owned by wantUid.
func verifyPipe(path string, wantUid int) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("audacity: pipe %q not found: %w (start Audacity, enable mod-script-pipe in Preferences → Modules, restart)", path, err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		return fmt.Errorf("audacity: %q is not a FIFO (mode %v)", path, fi.Mode())
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fmt.Errorf("audacity: cannot verify ownership of %q", path)
	}
	if int(stat.Uid) != wantUid {
		return fmt.Errorf("audacity: pipe %q owned by uid %d, want %d (same-user check)", path, stat.Uid, wantUid)
	}
	return nil
}

// mkFifo creates a FIFO for tests; platform-specific so the test file
// cross-compiles on Windows.
func mkFifo(path string, mode uint32) error { return syscall.Mkfifo(path, mode) }

// pollInterval is the bounded wait between non-blocking retries. The pipe
// is a local same-user FIFO, so a few milliseconds of latency per retry is
// invisible next to Audacity's own command latency, and every wait is a
// select on ctx.Done() — nothing ever blocks in the kernel.
const pollInterval = 2 * time.Millisecond

// fifo is one end of a same-user FIFO held on a raw, O_NONBLOCK descriptor.
//
// Raw descriptors (not *os.File) on purpose: Go's runtime poller treats
// FIFOs differently per platform (pollable on Linux, explicitly not on
// darwin — see go issue #24164), so a Close-to-interrupt or SetDeadline
// strategy is portable only by accident. With O_NONBLOCK on the raw fd,
// read(2)/write(2) return EAGAIN instead of blocking and every retry is a
// select on ctx.Done(): the same code is cancellable on both platforms, and
// no goroutine is ever parked in the kernel to be leaked.
type fifo struct {
	fd   int
	path string
}

// openFIFOForWrite opens path write-only. A FIFO with no reader fails the
// open with ENXIO; that is retried under ctx (bounded; no blocking open).
func openFIFOForWrite(ctx context.Context, path string) (*fifo, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			return &fifo{fd: fd, path: path}, nil
		}
		if !errors.Is(err, unix.ENXIO) && !errors.Is(err, unix.EINTR) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// openFIFOForRead opens path O_RDWR so the open never blocks waiting for a
// writer and so an idle pipe reports EAGAIN rather than EOF (with O_RDONLY
// an empty FIFO with no writer reads as EOF). Holding the write end
// ourselves is the standard FIFO trick; it never writes.
func openFIFOForRead(ctx context.Context, path string) (*fifo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &fifo{fd: fd, path: path}, nil
}

func (f *fifo) Close() error {
	if f == nil || f.fd < 0 {
		return nil
	}
	err := unix.Close(f.fd)
	f.fd = -1
	return err
}

// writeAll writes p completely, retrying EAGAIN/EINTR under ctx.
func (f *fifo) writeAll(ctx context.Context, p []byte) error {
	for len(p) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Write(f.fd, p)
		if n > 0 {
			p = p[n:]
			continue
		}
		if err == nil || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}
		return err
	}
	return nil
}

// readSome reads at least one byte into buf, waiting (bounded by ctx) while
// the FIFO is empty. It returns io.EOF only if the descriptor reports a
// zero-length read, which cannot happen on an O_RDWR FIFO end.
func (f *fifo) readSome(ctx context.Context, buf []byte) (int, error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, err := unix.Read(f.fd, buf)
		if n > 0 {
			return n, nil
		}
		if err == nil {
			return 0, errEOF
		}
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}
		return 0, err
	}
}
