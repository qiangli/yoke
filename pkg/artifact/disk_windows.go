// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build windows

package artifact

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modkernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

// DiskFreeBytes returns available free bytes for the current caller on the
// volume containing path (or its nearest existing ancestor directory).
func DiskFreeBytes(path string) (uint64, error) {
	target, err := nearestExistingDir(path)
	if err != nil {
		return 0, err
	}
	ptr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return 0, err
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	r1, _, e1 := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&freeBytesAvailable)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFreeBytes)),
	)
	if r1 == 0 {
		if e1 != nil && e1 != syscall.Errno(0) {
			return 0, fmt.Errorf("artifact: GetDiskFreeSpaceExW %s: %w", target, e1)
		}
		return 0, fmt.Errorf("artifact: GetDiskFreeSpaceExW %s failed", target)
	}
	return freeBytesAvailable, nil
}
