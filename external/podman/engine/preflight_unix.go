//go:build unix

package engine

import (
	"fmt"
	"syscall"
)

// freeDiskBytes returns free bytes on the partition holding `path`.
// Same implementation across all Unix variants — Statfs is in the
// platform syscall package on darwin/linux/bsd. (Windows uses
// GetDiskFreeSpaceEx; that lives in preflight_windows.go.)
func freeDiskBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Bavail = blocks available to a non-superuser. Bsize = block size.
	// Use unsigned conversions explicitly because Bsize is int32 on
	// darwin and uint32 on linux, and Bavail is uint64 on darwin and
	// int64 on linux. The product fits comfortably in uint64.
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
