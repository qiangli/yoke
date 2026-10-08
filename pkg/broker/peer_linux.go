//go:build linux

package broker

import "golang.org/x/sys/unix"

// peerCredentialsSupported: the kernel reports a unix peer's uid here.
const peerCredentialsSupported = true

// peerUID reads the peer's credentials from the kernel via SO_PEERCRED.
func peerUID(fd uintptr) (int, error) {
	cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}
