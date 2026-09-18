//go:build linux

package ask

import "golang.org/x/sys/unix"

// peerUIDFromFD reads the peer's credentials from the kernel via SO_PEERCRED.
func peerUIDFromFD(fd uintptr) (int, error) {
	cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}
