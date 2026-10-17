//go:build darwin || linux

package broker

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// shortDir keeps socket paths under the 104-byte sun_path limit on darwin.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "q8")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func perm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// A state dir left group/world-accessible and a stale world-writable socket
// from an earlier run are fixed at startup: 0700 dir, 0600 socket.
func TestOwnerSocketFixesLoosePermsAtStartup(t *testing.T) {
	dir := filepath.Join(shortDir(t), "broker")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "door.sock")
	stale, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	stale.Close()
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}

	ln, err := listenOwnerSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if got := perm(t, dir); got != 0o700 {
		t.Fatalf("state dir mode %v, want 0700", got)
	}
	if got := perm(t, sock); got != 0o600 {
		t.Fatalf("socket mode %v, want 0600", got)
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("owner cannot dial the fixed socket: %v", err)
	}
	c.Close()
}

// Anything at the socket path that is not a socket is refused, untouched:
// the door never deletes a file it did not create.
func TestOwnerSocketRefusesNonSocket(t *testing.T) {
	dir := filepath.Join(shortDir(t), "broker")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "door.sock")
	if err := os.WriteFile(sock, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ln, err := listenOwnerSocket(sock); err == nil {
		ln.Close()
		t.Fatal("listened over a regular file")
	}
	if data, err := os.ReadFile(sock); err != nil || string(data) != "keep" {
		t.Fatalf("regular file disturbed: %q %v", data, err)
	}
}

// A symlinked state dir could point the owner-only socket anywhere; refuse it.
func TestOwnerSocketRefusesSymlinkedDir(t *testing.T) {
	base := shortDir(t)
	real := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "broker")
	if err := os.Symlink(real, dir); err != nil {
		t.Fatal(err)
	}
	if ln, err := listenOwnerSocket(filepath.Join(dir, "door.sock")); err == nil {
		ln.Close()
		t.Fatal("listened in a symlinked state dir")
	}
	if got := perm(t, real); got != 0o755 {
		t.Fatalf("symlink target chmodded to %v", got)
	}
}
