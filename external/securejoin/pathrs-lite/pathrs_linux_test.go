//go:build linux

package pathrs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// fixture builds a root with in-root content, an outside directory
// holding a secret, and every symlink shape a hostile container image
// can plant to escape: absolute, relative ../ chains, a dangling
// absolute link, and a loop.
func fixture(t *testing.T) (root, outside string) {
	t.Helper()
	root = t.TempDir()
	outside = t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(outside, "secret"), []byte("OUTSIDE"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "etc"), 0o755))
	must(os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("INSIDE"), 0o644))
	must(os.WriteFile(filepath.Join(root, "secret"), []byte("ROOTED"), 0o644))
	must(os.Symlink(outside, filepath.Join(root, "abs-escape")))
	must(os.Symlink("../../../../../../../"+strings.TrimPrefix(outside, "/"), filepath.Join(root, "rel-escape")))
	must(os.Symlink("/etc/passwd", filepath.Join(root, "abs-passwd")))
	must(os.Symlink("etc", filepath.Join(root, "good")))
	must(os.Symlink("/etc", filepath.Join(root, "abs-etc")))
	must(os.Symlink("../../../..", filepath.Join(root, "rel-up")))
	must(os.Symlink("loop-b", filepath.Join(root, "loop-a")))
	must(os.Symlink("loop-a", filepath.Join(root, "loop-b")))
	return root, outside
}

func readAll(t *testing.T, f *os.File) string {
	t.Helper()
	rf, err := Reopen(f, unix.O_RDONLY)
	if err != nil {
		t.Fatalf("Reopen(%s): %v", f.Name(), err)
	}
	defer rf.Close()
	b, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fdPath is the kernel's view of where a handle actually points.
func fdPath(t *testing.T, f *os.File) string {
	t.Helper()
	p, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func within(root, p string) bool {
	root, _ = filepath.EvalSymlinks(root)
	return p == root || strings.HasPrefix(p, root+"/")
}

func TestOpenInRootResolvesInside(t *testing.T) {
	root, _ := fixture(t)
	for path, want := range map[string]string{
		"etc/passwd":             "INSIDE",
		"/etc/passwd":            "INSIDE",
		"good/passwd":            "INSIDE",
		"../../../etc/passwd":    "INSIDE",
		"abs-passwd":             "INSIDE",
		"abs-etc/../secret":      "ROOTED",
		"rel-up/etc/passwd":      "INSIDE",
		"etc/../../../../secret": "ROOTED",
		"./etc//./passwd":        "INSIDE",
	} {
		f, err := OpenInRoot(root, path)
		if err != nil {
			t.Errorf("OpenInRoot(%q): %v", path, err)
			continue
		}
		if !within(root, fdPath(t, f)) {
			t.Errorf("OpenInRoot(%q) handle points at %s, outside %s", path, fdPath(t, f), root)
		}
		if got := readAll(t, f); got != want {
			t.Errorf("OpenInRoot(%q) read %q, want %q", path, got, want)
		}
		f.Close()
	}
}

// TestOpenInRootSymlinkEscape is the FD-confinement contract: a symlink
// pointing outside the root is re-scoped under it, so a target that
// does not exist inside the root is ENOENT rather than the outside file.
func TestOpenInRootSymlinkEscape(t *testing.T) {
	root, outside := fixture(t)
	for _, path := range []string{"abs-escape/secret", "rel-escape/secret"} {
		f, err := OpenInRoot(root, path)
		if err == nil {
			got := fdPath(t, f)
			f.Close()
			if !within(root, got) {
				t.Fatalf("OpenInRoot(%q) escaped to %s (outside dir %s)", path, got, outside)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("OpenInRoot(%q) error = %v, want ENOENT", path, err)
		}
	}
}

func TestOpenInRootLoop(t *testing.T) {
	root, _ := fixture(t)
	if f, err := OpenInRoot(root, "loop-a"); err == nil {
		f.Close()
		t.Fatal("OpenInRoot on a symlink loop succeeded")
	} else if !errors.Is(err, unix.ELOOP) {
		t.Errorf("loop error = %v, want ELOOP", err)
	}
}

func TestOpenatInRootUsesHandle(t *testing.T) {
	root, _ := fixture(t)
	dir, err := os.OpenFile(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	// Move the root after opening it: resolution must follow the
	// handle, not the stale pathname.
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(moved, root)
	f, err := OpenatInRoot(dir, "abs-passwd")
	if err != nil {
		t.Fatalf("OpenatInRoot: %v", err)
	}
	defer f.Close()
	if got := readAll(t, f); got != "INSIDE" {
		t.Errorf("read %q, want INSIDE", got)
	}
}

func TestOpenatInRootFallbackWalk(t *testing.T) {
	root, _ := fixture(t)
	dir, err := os.OpenFile(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	for path, want := range map[string]string{
		"abs-passwd":           "INSIDE",
		"rel-up/etc/passwd":    "INSIDE",
		"abs-etc/../secret":    "ROOTED",
		"good/../../../secret": "ROOTED",
	} {
		f, err := walkInRoot(dir, path, walkOpts{})
		if err != nil {
			t.Errorf("walkInRoot(%q): %v", path, err)
			continue
		}
		if got := readAll(t, f); got != want {
			t.Errorf("walkInRoot(%q) read %q, want %q", path, got, want)
		}
		f.Close()
	}
	if f, err := walkInRoot(dir, "abs-escape/secret", walkOpts{}); err == nil {
		got := fdPath(t, f)
		f.Close()
		t.Fatalf("walkInRoot escaped to %s", got)
	}
	if _, err := walkInRoot(dir, "loop-a", walkOpts{}); !errors.Is(err, unix.ELOOP) {
		t.Errorf("walkInRoot loop error = %v, want ELOOP", err)
	}
}

func TestReopenSameInode(t *testing.T) {
	root, _ := fixture(t)
	f, err := OpenInRoot(root, "etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rf, err := Reopen(f, unix.O_RDONLY)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	var a, b unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &a); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(int(rf.Fd()), &b); err != nil {
		t.Fatal(err)
	}
	if a.Dev != b.Dev || a.Ino != b.Ino {
		t.Errorf("Reopen changed inode: %d/%d -> %d/%d", a.Dev, a.Ino, b.Dev, b.Ino)
	}
	if flags, err := unix.FcntlInt(rf.Fd(), unix.F_GETFL, 0); err != nil || flags&unix.O_PATH != 0 {
		t.Errorf("Reopen result flags = %#x, %v; want a real (non-O_PATH) handle", flags, err)
	}
}

func TestMkdirAllHandleStaysInRoot(t *testing.T) {
	root, outside := fixture(t)
	dir, err := os.OpenFile(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	d, err := MkdirAllHandle(dir, "abs-escape/new/deeper", 0o755)
	if err != nil {
		t.Fatalf("MkdirAllHandle: %v", err)
	}
	defer d.Close()
	if got := fdPath(t, d); !within(root, got) {
		t.Fatalf("MkdirAllHandle created at %s, outside root", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MkdirAllHandle created a directory outside the root: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(root, outside, "new", "deeper")); err != nil || !fi.IsDir() {
		t.Errorf("expected re-scoped directory under root: %v", err)
	}
	if _, err := MkdirAllHandle(dir, "etc/passwd/sub", 0o755); !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("MkdirAllHandle through a file: %v, want ENOTDIR", err)
	}
	if _, err := MkdirAllHandle(dir, "x", os.ModeDir|0o755); err == nil {
		t.Error("MkdirAllHandle accepted non-permission mode bits")
	}
}
