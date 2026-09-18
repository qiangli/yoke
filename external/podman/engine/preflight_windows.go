//go:build windows

package engine

import (
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// freeMemoryMB reports available and total physical memory. It lets the Podman
// machine defaults scale on Windows instead of falling back to the historical
// 2 CPU / 4 GiB / 50 GiB sizing.
func freeMemoryMB() (free uint64, total uint64, err error) {
	var s memoryStatusEx
	s.Length = uint32(unsafe.Sizeof(s))
	r1, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&s)))
	if r1 == 0 {
		if callErr != syscall.Errno(0) {
			return 0, 0, callErr
		}
		return 0, 0, syscall.EINVAL
	}
	if s.TotalPhys == 0 {
		return 0, 0, syscall.EINVAL
	}
	return s.AvailPhys / (1024 * 1024), s.TotalPhys / (1024 * 1024), nil
}

// freeDiskBytes reports free bytes on the volume containing path.
func freeDiskBytes(path string) (uint64, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	root := filepath.VolumeName(abs)
	if root == "" {
		root = abs
	} else {
		root += `\`
	}
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, err
	}
	var freeAvailable, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvailable, &totalBytes, &totalFree); err != nil {
		return 0, err
	}
	return freeAvailable, nil
}
