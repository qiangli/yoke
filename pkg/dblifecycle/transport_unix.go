//go:build !windows

package dblifecycle

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

func socketDir() string {
	if p := os.Getenv("BASHY_DB_SOCKET_DIR"); p != "" {
		return p
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("bashy-db-%d", os.Getuid()))
}

func listenOwner(id string) (net.Listener, string, error) {
	dir := socketDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, id+".sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, "", err
	}
	return ln, path, nil
}

func dialOwner(ctx context.Context, address string) (net.Conn, error) {
	dir := socketDir() + string(filepath.Separator)
	if !strings.HasPrefix(address, dir) || filepath.Base(address) == "" {
		return nil, fmt.Errorf("owner IPC address outside protected socket directory")
	}
	d := net.Dialer{}
	return d.DialContext(ctx, "unix", address)
}

func removeOwnerAddress(address string) { _ = os.Remove(address) }

func ownerToken() (string, error) { return randomToken() }
