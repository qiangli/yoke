// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build !windows

package artifact

import (
	"fmt"
	"syscall"
)

// DiskFreeBytes returns available free bytes for unprivileged callers on the
// filesystem containing path (or its nearest existing ancestor directory).
func DiskFreeBytes(path string) (uint64, error) {
	target, err := nearestExistingDir(path)
	if err != nil {
		return 0, err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(target, &st); err != nil {
		return 0, fmt.Errorf("artifact: statfs %s: %w", target, err)
	}
	if st.Bsize <= 0 {
		return 0, fmt.Errorf("artifact: invalid statfs block size %d for %s", st.Bsize, target)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
