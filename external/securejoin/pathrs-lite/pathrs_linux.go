//go:build linux

package pathrs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// maxSymlinks matches the kernel's MAXSYMLINKS budget for a single
// path resolution.
const maxSymlinks = 40

// OpenInRoot opens unsafePath inside root, resolving every symlink and
// ".." as if root were "/". The result is an O_PATH handle.
func OpenInRoot(root, unsafePath string) (*os.File, error) {
	dir, err := os.OpenFile(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return OpenatInRoot(dir, unsafePath)
}

// OpenatInRoot is [OpenInRoot] relative to an already-open root
// directory handle; the handle, not its pathname, bounds the lookup.
func OpenatInRoot(root *os.File, unsafePath string) (*os.File, error) {
	how := &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := openat2(int(root.Fd()), unsafePath, how)
	runtime.KeepAlive(root)
	if errors.Is(err, errNoOpenat2) {
		return walkInRoot(root, unsafePath, walkOpts{})
	}
	if err != nil {
		return nil, &os.PathError{Op: "openat2", Path: joinName(root, unsafePath), Err: err}
	}
	return os.NewFile(uintptr(fd), joinName(root, unsafePath)), nil
}

// MkdirAllHandle creates every missing directory of unsafePath inside
// root (resolved with in-root semantics, like [OpenatInRoot]) and
// returns an O_PATH handle to the final directory. mode carries only
// permission, sticky, setuid and setgid bits.
func MkdirAllHandle(root *os.File, unsafePath string, mode os.FileMode) (*os.File, error) {
	if mode&^(os.ModePerm|os.ModeSticky|os.ModeSetuid|os.ModeSetgid) != 0 {
		return nil, fmt.Errorf("mkdirall: unsupported mode bits in %s", mode)
	}
	perm := uint32(mode.Perm())
	if mode&os.ModeSticky != 0 {
		perm |= unix.S_ISVTX
	}
	if mode&os.ModeSetuid != 0 {
		perm |= unix.S_ISUID
	}
	if mode&os.ModeSetgid != 0 {
		perm |= unix.S_ISGID
	}
	return walkInRoot(root, unsafePath, walkOpts{mkdir: true, mode: perm})
}

// Reopen turns a handle (typically O_PATH) into a real open file with
// flags, by reopening the kernel's own reference to it through a
// verified procfs. The result is checked to be the same inode.
func Reopen(file *os.File, flags int) (*os.File, error) {
	proc, err := openProcRootFile()
	if err != nil {
		return nil, err
	}
	defer proc.Close()
	fdDir, err := openInProc(proc, "self/fd")
	if err != nil {
		return nil, err
	}
	defer fdDir.Close()

	// The handle is already resolved; O_NOFOLLOW would only make the
	// magic link itself refuse to open, and O_CREAT is meaningless.
	flags &^= unix.O_NOFOLLOW | unix.O_CREAT | unix.O_EXCL
	name := strconv.Itoa(int(file.Fd()))
	fd, err := unix.Openat(int(fdDir.Fd()), name, flags|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "reopen", Path: file.Name(), Err: err}
	}
	var want, got unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &want); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: file.Name(), Err: err}
	}
	runtime.KeepAlive(file)
	if err := unix.Fstat(fd, &got); err != nil {
		unix.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: file.Name(), Err: err}
	}
	if want.Dev != got.Dev || want.Ino != got.Ino {
		unix.Close(fd)
		return nil, fmt.Errorf("reopen %s: handle changed identity during reopen", file.Name())
	}
	return os.NewFile(uintptr(fd), file.Name()), nil
}

type walkOpts struct {
	mkdir bool
	mode  uint32
}

// walkInRoot resolves unsafePath beneath root one component at a time
// with openat(O_PATH|O_NOFOLLOW). It keeps a handle to every directory
// on the current path; ".." pops back to the held parent and stops at
// root, and an absolute symlink target restarts from root, so nothing
// is ever opened by a joined pathname that could escape. With
// opts.mkdir, missing components are created and the result must be a
// directory.
func walkInRoot(root *os.File, unsafePath string, opts walkOpts) (_ *os.File, Err error) {
	name := joinName(root, unsafePath)
	fail := func(op string, err error) (*os.File, error) {
		return nil, &os.PathError{Op: op, Path: name, Err: err}
	}
	rootfd, err := unix.FcntlInt(root.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	runtime.KeepAlive(root)
	if err != nil {
		return fail("dup", err)
	}
	stack := []int{rootfd}
	defer func() {
		// On success the top was handed off and removed from stack.
		for _, fd := range stack {
			unix.Close(fd)
		}
	}()

	pending := splitPath(unsafePath)
	links := 0
	for len(pending) > 0 {
		c := pending[0]
		pending = pending[1:]
		switch c {
		case ".":
			continue
		case "..":
			if len(stack) > 1 {
				unix.Close(stack[len(stack)-1])
				stack = stack[:len(stack)-1]
			}
			continue
		}
		top := stack[len(stack)-1]
		fd, err := unix.Openat(top, c, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && opts.mkdir {
			if err = unix.Mkdirat(top, c, opts.mode); err != nil && !errors.Is(err, unix.EEXIST) {
				return fail("mkdirat", err)
			}
			fd, err = unix.Openat(top, c, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return fail("openat", err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return fail("fstat", err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFLNK:
			target, err := readlinkat(fd, "")
			unix.Close(fd)
			if err != nil {
				return fail("readlinkat", err)
			}
			if links++; links > maxSymlinks {
				return fail("openat", unix.ELOOP)
			}
			if strings.HasPrefix(target, "/") {
				for _, d := range stack[1:] {
					unix.Close(d)
				}
				stack = stack[:1]
			}
			pending = append(splitPath(target), pending...)
			continue
		case unix.S_IFDIR:
		default:
			if len(pending) > 0 || opts.mkdir {
				unix.Close(fd)
				return fail("openat", unix.ENOTDIR)
			}
		}
		stack = append(stack, fd)
	}
	top := stack[len(stack)-1]
	stack = stack[:len(stack)-1]
	return os.NewFile(uintptr(top), name), nil
}

func splitPath(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" {
			out = append(out, c)
		}
	}
	return out
}

// joinName is the display name of a handle opened inside root; it is
// never used to open anything.
func joinName(root *os.File, unsafePath string) string {
	return filepath.Join(root.Name(), filepath.Clean("/"+unsafePath))
}
