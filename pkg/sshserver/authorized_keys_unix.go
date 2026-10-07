//go:build !windows

package sshserver

import (
	"os"
	"syscall"
)

func authorizedKeysOwnedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
