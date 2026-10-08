package broker

import (
	"net"
	"os"
)

// Socket mode is insufficient on BSD systems; authenticate kernel credentials.
func socketOwner(c net.Conn) bool {
	u, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := u.SyscallConn()
	if err != nil {
		return false
	}
	uid := -1
	var probeErr error
	if err := raw.Control(func(fd uintptr) { uid, probeErr = peerUID(fd) }); err != nil {
		return false
	}
	return probeErr == nil && uid == os.Geteuid()
}
