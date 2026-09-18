//go:build windows

package bus

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

func parentProcessID(pid int) (int, bool) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, false
	}
	for {
		if int(entry.ProcessID) == pid {
			return int(entry.ParentProcessID), true
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			return 0, false
		}
	}
}
