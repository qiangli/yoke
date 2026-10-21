//go:build linux

package procfs

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// Handle is a verified handle to the root of a procfs mount. Every
// Open method resolves beneath it with file-descriptor based lookups,
// so the result can neither leave /proc, cross into a different mount
// planted over a procfs file, nor follow a magic link.
type Handle struct {
	root *os.File
}

// OpenProcRoot opens the host /proc after checking it is the root of
// a genuine procfs mount.
func OpenProcRoot() (*Handle, error) {
	f, err := openProcRootFile()
	if err != nil {
		return nil, err
	}
	return &Handle{root: f}, nil
}

// OpenUnsafeProcRoot is [OpenProcRoot]; this implementation applies
// the same procfs verification to both.
func OpenUnsafeProcRoot() (*Handle, error) { return OpenProcRoot() }

// Close releases the handle. It is safe on a nil handle.
func (h *Handle) Close() error {
	if h == nil || h.root == nil {
		return nil
	}
	err := h.root.Close()
	h.root = nil
	return err
}

func (h *Handle) open(p string) (*os.File, error) {
	if h == nil || h.root == nil {
		return nil, &os.PathError{Op: "procfs", Path: p, Err: os.ErrClosed}
	}
	return openInProc(h.root, p)
}

// OpenSelf opens /proc/self/<subpath> as an O_PATH handle.
func (h *Handle) OpenSelf(subpath string) (*os.File, error) {
	return h.open("self/" + subpath)
}

// OpenPid opens /proc/<pid>/<subpath> as an O_PATH handle.
func (h *Handle) OpenPid(pid int, subpath string) (*os.File, error) {
	if pid <= 0 {
		return nil, &os.PathError{Op: "procfs", Path: strconv.Itoa(pid), Err: unix.EINVAL}
	}
	return h.open(strconv.Itoa(pid) + "/" + subpath)
}

// OpenThreadSelf opens /proc/thread-self/<subpath> as an O_PATH
// handle. The calling goroutine stays locked to its OS thread until the
// returned closer runs, because the handle names that thread.
func (h *Handle) OpenThreadSelf(subpath string) (*os.File, ProcThreadSelfCloser, error) {
	runtime.LockOSThread()
	var once sync.Once
	closer := ProcThreadSelfCloser(func() { once.Do(runtime.UnlockOSThread) })

	prefix := "thread-self/"
	if h != nil && h.root != nil && !hasThreadSelf(h.root) {
		// Pre-3.17 kernels have no thread-self link.
		prefix = "self/task/" + strconv.Itoa(unix.Gettid()) + "/"
	}
	f, err := h.open(prefix + subpath)
	if err != nil {
		closer()
		return nil, nil, err
	}
	return f, closer, nil
}

func hasThreadSelf(root *os.File) bool {
	var st unix.Stat_t
	err := unix.Fstatat(int(root.Fd()), "thread-self", &st, unix.AT_SYMLINK_NOFOLLOW)
	return !errors.Is(err, unix.ENOENT)
}
