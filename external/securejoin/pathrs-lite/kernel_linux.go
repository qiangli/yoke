// Code shared verbatim between pathrs-lite and pathrs-lite/procfs:
// the two packages must not import each other or an internal package,
// because this module is compiled under two module paths (as the
// cyphar/filepath-securejoin replacement and standalone). Edit both
// copies together; TestKernelCopiesInSync enforces it.

package pathrs

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// maxRetries bounds the EAGAIN/EINTR loop around openat2. The kernel
// returns EAGAIN from RESOLVE_IN_ROOT/RESOLVE_BENEATH lookups when a
// concurrent rename or mount could have changed the result.
const maxRetries = 128

// noOpenat2 records that the running kernel lacks openat2 (ENOSYS,
// pre-5.6), so callers go straight to their openat walk.
var noOpenat2 atomic.Bool

// errNoOpenat2 reports that openat2(2) is unavailable on this kernel.
var errNoOpenat2 = errors.New("openat2 unavailable")

// openat2 calls openat2(2), retrying transient EAGAIN/EINTR. It
// returns errNoOpenat2 (wrapped) when the syscall does not exist.
func openat2(dirfd int, p string, how *unix.OpenHow) (int, error) {
	if noOpenat2.Load() {
		return -1, errNoOpenat2
	}
	var err error
	for i := 0; i < maxRetries; i++ {
		var fd int
		fd, err = unix.Openat2(dirfd, p, how)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.ENOSYS):
			noOpenat2.Store(true)
			return -1, errNoOpenat2
		case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
			continue
		}
		return -1, err
	}
	return -1, err
}

// procRootIno is the inode number procfs gives its root directory
// (PROC_ROOT_INO in proc(5)).
const procRootIno = 1

// isProcfs verifies fd lives on a procfs filesystem.
func isProcfs(fd int) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(fd, &st); err != nil {
		return &os.PathError{Op: "fstatfs", Path: "/proc", Err: err}
	}
	if st.Type != unix.PROC_SUPER_MAGIC {
		return fmt.Errorf("refusing non-procfs filesystem (f_type %#x) where procfs was expected", st.Type)
	}
	return nil
}

// openProcRootFile opens /proc as an O_PATH directory handle and verifies
// it is the root of a real procfs mount, so a bind mount or tmpfs
// planted over /proc cannot feed us forged files.
func openProcRootFile() (*os.File, error) {
	fd, err := unix.Open("/proc", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: "/proc", Err: err}
	}
	if err := isProcfs(fd); err != nil {
		unix.Close(fd)
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: "/proc", Err: err}
	}
	if st.Ino != procRootIno {
		unix.Close(fd)
		return nil, fmt.Errorf("/proc is not the root of a procfs mount (inode %d)", st.Ino)
	}
	return os.NewFile(uintptr(fd), "/proc"), nil
}

// cleanProcPath validates a path relative to the proc root: no ".."
// components (procfs lookups never need them, and they are how a
// subpath would hop to another pid or out of /proc), and no empty
// result. A leading "/" is treated as relative to the proc root.
func cleanProcPath(p string) (string, error) {
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return "", &os.PathError{Op: "procfs", Path: p, Err: unix.EXDEV}
		}
	}
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" {
		return "", &os.PathError{Op: "procfs", Path: p, Err: unix.EINVAL}
	}
	return clean, nil
}

// openInProc opens p beneath the verified proc root as an O_PATH
// handle. The lookup never leaves the proc mount, never crosses into
// another mount (an overmounted procfs file is refused), and never
// follows magic links such as cwd, root, exe or fd/N; the ordinary
// self and thread-self symlinks still resolve.
func openInProc(proc *os.File, p string) (*os.File, error) {
	clean, err := cleanProcPath(p)
	if err != nil {
		return nil, err
	}
	name := "/proc/" + clean
	how := &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV,
	}
	fd, err := openat2(int(proc.Fd()), clean, how)
	if errors.Is(err, errNoOpenat2) {
		fd, err = walkProc(int(proc.Fd()), clean)
	}
	if err != nil {
		return nil, &os.PathError{Op: "openat2", Path: name, Err: unwrapPathErr(err)}
	}
	return os.NewFile(uintptr(fd), name), nil
}

func unwrapPathErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// walkProc is the pre-openat2 fallback: one openat(O_NOFOLLOW) per
// component, with every intermediate handle re-checked as procfs. Only
// the leading self/thread-self symlink is resolved (its target is a
// plain relative pid path); any other symlink is refused with ELOOP,
// which excludes every magic link.
func walkProc(procfd int, clean string) (int, error) {
	comps := strings.Split(clean, "/")
	if comps[0] == "self" || comps[0] == "thread-self" {
		target, err := readlinkat(procfd, comps[0])
		if err != nil {
			return -1, err
		}
		if strings.HasPrefix(target, "/") || strings.Contains("/"+target+"/", "/../") {
			return -1, unix.EXDEV
		}
		comps = append(strings.Split(target, "/"), comps[1:]...)
	}
	cur, err := unix.Openat(procfd, ".", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, c := range comps {
		if c == "" || c == "." {
			continue
		}
		next, err := unix.Openat(cur, c, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(cur)
		if err != nil {
			return -1, err
		}
		cur = next
		if err := isProcfs(cur); err != nil {
			unix.Close(cur)
			return -1, unix.EXDEV
		}
		var st unix.Stat_t
		if err := unix.Fstat(cur, &st); err != nil {
			unix.Close(cur)
			return -1, err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			unix.Close(cur)
			return -1, unix.ELOOP
		}
	}
	return cur, nil
}

// readlinkat is readlinkat(2) with a growing buffer. An empty name
// reads the symlink an O_PATH|O_NOFOLLOW dirfd refers to.
func readlinkat(dirfd int, name string) (string, error) {
	for size := 128; ; size *= 2 {
		buf := make([]byte, size)
		n, err := unix.Readlinkat(dirfd, name, buf)
		if err != nil {
			return "", err
		}
		if n < size {
			return string(buf[:n]), nil
		}
	}
}
