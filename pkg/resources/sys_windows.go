//go:build windows

package resources

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Windows reads through the documented Win32/NT entry points via
// golang.org/x/sys/windows — lazy DLL binding, no cgo:
//
//	CPU        NtQuerySystemInformation(SystemProcessorPerformanceInformation)
//	           for per-core ticks; the aggregate is their sum.
//	memory     GlobalMemoryStatusEx
//	disks      GetLogicalDrives + GetDiskFreeSpaceEx (fixed drives only)
//	network    GetIfTable (MIB_IFROW octet counters)
//	GPU        the display-class registry key's DriverDesc +
//	           HardwareInformation.qwMemorySize
//
// Disk IO rates are not collected here: the per-volume counters live in
// the PDH performance library, and a partial-but-wrong number is worse
// than an omitted one.

var (
	ntdll                        = windows.NewLazySystemDLL("ntdll.dll")
	procNtQuerySystemInformation = ntdll.NewProc("NtQuerySystemInformation")

	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIfTable      = iphlpapi.NewProc("GetIfTable")
	kernel32            = windows.NewLazySystemDLL("kernel32.dll")
	procGetDiskFree     = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGlobalMemStatus = kernel32.NewProc("GlobalMemoryStatusEx")
)

// memoryStatusEx mirrors MEMORYSTATUSEX (x/sys/windows does not export it).
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

// systemProcessorPerformanceInformation is class 8 of
// NtQuerySystemInformation: one record per logical processor.
type systemProcessorPerformanceInformation struct {
	IdleTime       int64
	KernelTime     int64 // includes IdleTime
	UserTime       int64
	DpcTime        int64
	InterruptTime  int64
	InterruptCount uint32
	_              uint32 // trailing alignment
}

const systemProcessorPerformanceInformationClass = 8

func sampleCounters() (counters, error) {
	c := counters{at: time.Now()}
	cores, err := processorTimes()
	if err != nil {
		c.warnings = append(c.warnings, "cpu: "+err.Error())
	} else {
		c.hasCPU = true
		for _, t := range cores {
			// KernelTime already contains IdleTime, so total is
			// kernel+user and the idle share is IdleTime.
			core := cpuTicks{total: uint64(t.KernelTime + t.UserTime), idle: uint64(t.IdleTime)}
			c.perCore = append(c.perCore, core)
			c.cpu.total += core.total
			c.cpu.idle += core.idle
		}
	}
	nics, err := interfaceCounters()
	if err != nil {
		c.warnings = append(c.warnings, "network: "+err.Error())
	}
	c.net = nics
	return c, nil
}

func processorTimes() ([]systemProcessorPerformanceInformation, error) {
	n := runtime.NumCPU()
	size := int(unsafe.Sizeof(systemProcessorPerformanceInformation{}))
	buf := make([]systemProcessorPerformanceInformation, n)
	var returned uint32
	status, _, _ := procNtQuerySystemInformation.Call(
		uintptr(systemProcessorPerformanceInformationClass),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(size*n),
		uintptr(unsafe.Pointer(&returned)),
	)
	if status != 0 {
		return nil, fmt.Errorf("NtQuerySystemInformation: status 0x%x", status)
	}
	got := int(returned) / size
	if got > n {
		got = n
	}
	return buf[:got], nil
}

func cpuStatic() (string, int, int) {
	model := ""
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DESCRIPTION\System\CentralProcessor\0`, registry.QUERY_VALUE); err == nil {
		if s, _, err := k.GetStringValue("ProcessorNameString"); err == nil {
			model = strings.TrimSpace(s)
		}
		k.Close()
	}
	// Physical core count needs GetLogicalProcessorInformationEx; the
	// logical count is what schedules work, so physical stays omitted.
	return model, runtime.NumCPU(), 0
}

// loadAverage: Windows has no run-queue average. Omitted, not faked.
func loadAverage() ([]float64, bool) { return nil, false }

func memoryStats() (Memory, error) {
	var st memoryStatusEx
	st.Length = uint32(unsafe.Sizeof(st))
	if ret, _, err := procGlobalMemStatus.Call(uintptr(unsafe.Pointer(&st))); ret == 0 {
		return Memory{}, fmt.Errorf("GlobalMemoryStatusEx: %w", err)
	}
	m := Memory{
		TotalBytes:     st.TotalPhys,
		AvailableBytes: st.AvailPhys,
		Source:         "GlobalMemoryStatusEx",
	}
	if st.TotalPhys > st.AvailPhys {
		m.UsedBytes = st.TotalPhys - st.AvailPhys
	}
	// The page file totals are commit limits, not physical swap: report the
	// portion beyond physical memory as swap.
	if st.TotalPageFile > st.TotalPhys {
		m.SwapTotalBytes = st.TotalPageFile - st.TotalPhys
		free := uint64(0)
		if st.AvailPageFile > st.AvailPhys {
			free = st.AvailPageFile - st.AvailPhys
		}
		if m.SwapTotalBytes > free {
			m.SwapUsedBytes = m.SwapTotalBytes - free
		}
	}
	return m, nil
}

func diskStats() ([]Disk, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, err
	}
	var out []Disk
	for i := range 26 {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, err := windows.UTF16PtrFromString(root)
		if err != nil {
			continue
		}
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		var availCaller, total, totalFree uint64
		ret, _, _ := procGetDiskFree.Call(uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&availCaller)),
			uintptr(unsafe.Pointer(&total)),
			uintptr(unsafe.Pointer(&totalFree)))
		if ret == 0 || total == 0 {
			continue
		}
		d := Disk{Mount: root, Device: root[:2], FSType: volumeFSType(root), TotalBytes: total, FreeBytes: availCaller}
		if total > totalFree {
			d.UsedBytes = total - totalFree
		}
		d.UsedPercent = pct(d.UsedBytes, d.UsedBytes+d.FreeBytes)
		out = append(out, d)
	}
	return out, nil
}

func volumeFSType(root string) string {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return ""
	}
	nameBuf := make([]uint16, 261)
	fsBuf := make([]uint16, 261)
	var serial, maxComponent, flags uint32
	if err := windows.GetVolumeInformation(p, &nameBuf[0], uint32(len(nameBuf)), &serial, &maxComponent, &flags, &fsBuf[0], uint32(len(fsBuf))); err != nil {
		return ""
	}
	return windows.UTF16ToString(fsBuf)
}

// mibIfRow mirrors MIB_IFROW. Every member is 4-byte aligned, so the Go
// layout matches the C one exactly.
type mibIfRow struct {
	Name            [256]uint16
	Index           uint32
	Type            uint32
	Mtu             uint32
	Speed           uint32
	PhysAddrLen     uint32
	PhysAddr        [8]byte
	AdminStatus     uint32
	OperStatus      uint32
	LastChange      uint32
	InOctets        uint32
	InUcastPkts     uint32
	InNUcastPkts    uint32
	InDiscards      uint32
	InErrors        uint32
	InUnknownProtos uint32
	OutOctets       uint32
	OutUcastPkts    uint32
	OutNUcastPkts   uint32
	OutDiscards     uint32
	OutErrors       uint32
	OutQLen         uint32
	DescrLen        uint32
	Descr           [256]byte
}

func interfaceCounters() ([]netCounter, error) {
	var size uint32
	// First call sizes the table (ERROR_INSUFFICIENT_BUFFER expected).
	procGetIfTable.Call(0, uintptr(unsafe.Pointer(&size)), 0)
	if size == 0 {
		return nil, fmt.Errorf("GetIfTable: empty interface table")
	}
	buf := make([]byte, size)
	ret, _, _ := procGetIfTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0)
	if ret != 0 {
		return nil, fmt.Errorf("GetIfTable: error %d", ret)
	}
	count := *(*uint32)(unsafe.Pointer(&buf[0]))
	rowSize := int(unsafe.Sizeof(mibIfRow{}))
	var out []netCounter
	for i := range int(count) {
		off := 4 + i*rowSize
		if off+rowSize > len(buf) {
			break
		}
		row := (*mibIfRow)(unsafe.Pointer(&buf[off]))
		name := windows.UTF16ToString(row.Name[:])
		if name == "" {
			name = string(row.Descr[:row.DescrLen])
		}
		if name == "" {
			continue
		}
		out = append(out, netCounter{name: name, rxBytes: uint64(row.InOctets), txBytes: uint64(row.OutOctets)})
	}
	return out, nil
}

// displayClassGUID is the Windows setup class for display adapters.
const displayClassGUID = `SYSTEM\CurrentControlSet\Control\Class\{4d36e968-e325-11ce-bfc1-08002be10318}`

// gpuStats enumerates the display-adapter registry subkeys. Absence is
// normal (a headless server has none) and yields an empty slice.
func gpuStats() ([]GPU, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, displayClassGUID, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return nil, nil
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return nil, nil
	}
	var out []GPU
	for _, name := range names {
		// Adapter instances are the numeric subkeys (0000, 0001, ...).
		if len(name) != 4 || strings.Trim(name, "0123456789") != "" {
			continue
		}
		sub, err := registry.OpenKey(k, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		g := GPU{Source: filepath.Join("HKLM", displayClassGUID, name), VRAMKind: "dedicated"}
		if s, _, err := sub.GetStringValue("DriverDesc"); err == nil {
			g.Name = strings.TrimSpace(s)
		}
		if s, _, err := sub.GetStringValue("ProviderName"); err == nil {
			g.Vendor = strings.TrimSpace(s)
		}
		if v, _, err := sub.GetIntegerValue("HardwareInformation.qwMemorySize"); err == nil {
			g.VRAMBytes = v
		} else if b, _, err := sub.GetBinaryValue("HardwareInformation.MemorySize"); err == nil && len(b) >= 4 {
			g.VRAMBytes = uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24
		}
		sub.Close()
		if g.Name == "" {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}
