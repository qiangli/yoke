//go:build darwin

package ask

import "golang.org/x/sys/unix"

// peerUIDFromFD reads the peer's credentials from the kernel via LOCAL_PEERCRED
// (the BSD/darwin spelling of SO_PEERCRED).
//
// This matters more on darwin than on Linux: a unix socket's own mode is not
// consulted when a peer connects here, so the kernel credential is not merely the
// authoritative check, it is very nearly the only one.
func peerUIDFromFD(fd uintptr) (int, error) {
	xu, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return -1, err
	}
	return int(xu.Uid), nil
}
