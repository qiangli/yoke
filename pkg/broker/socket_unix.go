//go:build unix

package broker

import (
	"os"
	"syscall"
)

// ownedBySelf reports whether fi belongs to this process's effective user.
func ownedBySelf(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
