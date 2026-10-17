package broker

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
)

// listenOwnerSocket opens the door's owner-only unix socket: 0600 in a 0700
// directory (Q8). Wrong permissions left by an earlier run, a umask or a hand
// edit are fixed at startup; anything that cannot be fixed safely — a
// symlinked or foreign-owned directory, a non-socket or foreign-owned file at
// the socket path — is refused. The directory is tightened BEFORE listen, so
// the window between bind and chmod is unreachable for other users; kernel
// peer credentials (socketOwner) remain the per-connection check.
func listenOwnerSocket(sock string) (net.Listener, error) {
	dir := filepath.Dir(sock)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("broker: socket directory %s is not a directory (symlink?); refusing", dir)
	}
	if !ownedBySelf(fi) {
		return nil, fmt.Errorf("broker: socket directory %s is not owned by this user; refusing", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("broker: tighten %s to 0700: %w", dir, err)
		}
	}
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("broker: %s exists and is not a socket; refusing to replace it", sock)
		}
		if !ownedBySelf(fi) {
			return nil, fmt.Errorf("broker: socket %s is not owned by this user; refusing", sock)
		}
		if err := os.Remove(sock); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if fi, err := os.Lstat(sock); err != nil || fi.Mode().Perm() != 0o600 {
		ln.Close()
		return nil, fmt.Errorf("broker: socket %s is not 0600 after chmod; refusing", sock)
	}
	return ln, nil
}
