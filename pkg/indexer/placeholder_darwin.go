//go:build darwin

package indexer

import (
	"golang.org/x/sys/unix"
	"io/fs"
	"syscall"
)

func placeholder(st fs.FileInfo) bool {
	v, ok := st.Sys().(*syscall.Stat_t)
	return ok && v.Flags&unix.SF_DATALESS != 0
}
