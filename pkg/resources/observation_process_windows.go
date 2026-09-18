//go:build windows

package resources

import (
	"context"
	"fmt"
	"golang.org/x/sys/windows"
	"unsafe"
)

var observationProcessMemory = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

type observationMemoryCounters struct {
	Size, Faults                                                                           uint32
	PeakWorking, Working, PeakPaged, Paged, PeakNonpaged, Nonpaged, Pagefile, PeakPagefile uintptr
}

func collectProcessSamples(ctx context.Context) ([]processSample, ObservationCoverage, error) {
	ctx, cancel := processBudget(ctx)
	defer cancel()
	coverage := ObservationCoverage{Complete: true, Limit: maxObservedProcesses}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, coverage, err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	err = windows.Process32First(snapshot, &entry)
	var out []processSample
	for err == nil {
		coverage.Seen++
		if ctx.Err() != nil || len(out) >= maxObservedProcesses {
			coverage.Complete = false
			coverage.Reason = "process count or time limit"
			break
		}
		p := processSample{Identity: ProcessIdentity{PID: int(entry.ProcessID)}, PPID: int(entry.ParentProcessID), Name: windows.UTF16ToString(entry.ExeFile[:]), Source: "win32:process", Reason: "process access denied or unavailable"}
		handle, openErr := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, entry.ProcessID)
		if openErr == nil {
			var born, exit, kernel, user windows.Filetime
			if windows.GetProcessTimes(handle, &born, &exit, &kernel, &user) == nil {
				birth := uint64(born.HighDateTime)<<32 | uint64(born.LowDateTime)
				p.Identity.StartID = fmt.Sprintf("windows:%d", birth)
				cpu := (float64(uint64(kernel.HighDateTime)<<32|uint64(kernel.LowDateTime)) + float64(uint64(user.HighDateTime)<<32|uint64(user.LowDateTime))) / 1e7
				p.CPUSeconds = &cpu
			}
			mem := observationMemoryCounters{}
			mem.Size = uint32(unsafe.Sizeof(mem))
			result, _, _ := observationProcessMemory.Call(uintptr(handle), uintptr(unsafe.Pointer(&mem)), uintptr(mem.Size))
			if result != 0 {
				v := uint64(mem.Working)
				p.RSS = &v
			}
			windows.CloseHandle(handle)
		} else {
			coverage.Complete = false
			coverage.Reason = "some processes inaccessible"
		}
		out = append(out, p)
		err = windows.Process32Next(snapshot, &entry)
	}
	if err != nil && err != windows.ERROR_NO_MORE_FILES {
		return out, coverage, err
	}
	coverage.Included = len(out)
	return out, coverage, nil
}

func lookupNativeProcessIdentity(pid int) (ProcessIdentity, error) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ProcessIdentity{}, err
	}
	defer windows.CloseHandle(handle)
	var born, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &born, &exit, &kernel, &user); err != nil {
		return ProcessIdentity{}, err
	}
	birth := uint64(born.HighDateTime)<<32 | uint64(born.LowDateTime)
	return ProcessIdentity{PID: pid, StartID: fmt.Sprintf("windows:%d", birth)}, nil
}
