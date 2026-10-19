//go:build windows

package indexer

import (
	"golang.org/x/sys/windows"
	"io/fs"
	"syscall"
)

func placeholder(st fs.FileInfo) bool {
	v, ok := st.Sys().(*syscall.Win32FileAttributeData)
	return ok && v.FileAttributes&(windows.FILE_ATTRIBUTE_OFFLINE|windows.FILE_ATTRIBUTE_RECALL_ON_OPEN|windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS) != 0
}
