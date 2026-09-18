//go:build darwin

package resources

import (
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"strings"
	"time"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// darwin reads everything through sysctl(3) plus the routing socket's
// interface list. Two deliberate omissions, both because the only pure
// interface is a mach trap that needs cgo:
//
//   - Per-core (and aggregate) CPU ticks live in host_processor_info /
//     host_statistics. macOS exports no kern.cp_time sysctl, so the CPU
//     figure here is load-average-derived and carries Source "loadavg".
//   - GPU utilization and per-process VRAM live in IOKit. Apple silicon's
//     GPU is still reported — it is integrated, so its memory pool IS
//     hw.memsize, which sysctl does give us.

func sampleCounters() (counters, error) {
	c := counters{at: time.Now()}
	nics, err := darwinInterfaceCounters()
	if err != nil {
		c.warnings = append(c.warnings, "network: "+err.Error())
	}
	c.net = nics
	// No tick counters without cgo; buildCPU falls back to load average.
	c.hasCPU = false
	return c, nil
}

func cpuStatic() (string, int, int) {
	model, _ := unix.Sysctl("machdep.cpu.brand_string")
	physical := sysctlInt("hw.physicalcpu")
	return model, runtime.NumCPU(), physical
}

func loadAverage() ([]float64, bool) {
	// struct loadavg { fixpt_t ldavg[3]; long fscale; }
	b, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(b) < 24 {
		return nil, false
	}
	scale := float64(binary.LittleEndian.Uint64(b[16:24]))
	if scale == 0 {
		return nil, false
	}
	out := make([]float64, 0, 3)
	for i := range 3 {
		out = append(out, round(float64(binary.LittleEndian.Uint32(b[i*4:]))/scale, 2))
	}
	return out, true
}

func memoryStats() (Memory, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Memory{}, err
	}
	m := Memory{TotalBytes: total, Source: "sysctl"}

	// kern.memorystatus_level is macOS's own "percentage of memory still
	// available" signal — the one the kernel drives its pressure
	// notifications from. It is a truer availability figure than the free
	// page count, which on macOS is always near zero because the VM keeps
	// everything cached or compressed.
	if level := sysctlInt("kern.memorystatus_level"); level > 0 && level <= 100 {
		m.AvailableBytes = total / 100 * uint64(level)
		m.Source = "sysctl:memorystatus_level"
	} else {
		pageSize := sysctlInt("hw.pagesize")
		if pageSize <= 0 {
			pageSize = unix.Getpagesize()
		}
		page := uint64(pageSize)
		var free uint64
		for _, name := range []string{"vm.page_free_count", "vm.page_speculative_count"} {
			if pages := sysctlInt(name); pages > 0 {
				free += uint64(pages)
			}
		}
		m.AvailableBytes = free * page
		m.Source = "sysctl:page_free_count"
	}
	if total > m.AvailableBytes {
		m.UsedBytes = total - m.AvailableBytes
	}

	// struct xsw_usage { uint64 total; uint64 avail; uint64 used; ... }
	if b, err := unix.SysctlRaw("vm.swapusage"); err == nil && len(b) >= 24 {
		m.SwapTotalBytes = binary.LittleEndian.Uint64(b[0:8])
		m.SwapUsedBytes = binary.LittleEndian.Uint64(b[16:24])
	}
	return m, nil
}

func diskStats() ([]Disk, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	buf := make([]unix.Statfs_t, n)
	n, err = unix.Getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	var out []Disk
	for i := range n {
		st := &buf[i]
		fstype := unix.ByteSliceToString(st.Fstypename[:])
		device := unix.ByteSliceToString(st.Mntfromname[:])
		// Skip the synthetic mounts (devfs, autofs, the firmlink volumes
		// that re-report the data volume's numbers) — they double-count.
		if !strings.HasPrefix(device, "/dev/") {
			continue
		}
		d := diskFromStatfs(unix.ByteSliceToString(st.Mntonname[:]), device, fstype,
			uint64(st.Bsize), st.Blocks, st.Bfree, uint64(st.Bavail))
		if d.TotalBytes == 0 {
			continue
		}
		out = append(out, d)
	}
	// One APFS container backs the system, data, preboot, VM, and update
	// volumes, and every one of them reports the container's capacity. The
	// container — /dev/diskN out of /dev/diskNsMsK — is the real pool.
	return dedupeByKey(out, apfsContainer), nil
}

// apfsContainer reduces /dev/disk3s1s1 to /dev/disk3. A non-APFS device
// keys as itself, so nothing outside the container layout is collapsed.
func apfsContainer(d Disk) string {
	dev := d.Device
	if !strings.HasPrefix(dev, "/dev/disk") {
		return dev
	}
	if i := strings.IndexByte(dev[len("/dev/disk"):], 's'); i >= 0 {
		return dev[:len("/dev/disk")+i]
	}
	return dev
}

// gpuStats reports the integrated Apple silicon GPU. Its memory is the
// unified pool, which sysctl reports directly; discrete GPU enumeration
// (and any utilization figure) would require IOKit, i.e. cgo, so an Intel
// Mac reports no GPU rather than a guess.
func gpuStats() ([]GPU, error) {
	if sysctlInt("hw.optional.arm64") != 1 {
		return nil, nil
	}
	brand, _ := unix.Sysctl("machdep.cpu.brand_string")
	name := "Apple integrated GPU"
	if brand != "" {
		name = brand + " GPU"
	}
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return nil, err
	}
	return []GPU{{
		Vendor:    "Apple",
		Name:      name,
		VRAMBytes: total,
		VRAMKind:  "unified",
		Source:    "sysctl",
	}}, nil
}

// darwinInterfaceCounters parses the NET_RT_IFLIST2 routing dump. The
// message body is an if_msghdr2 whose ifm_data (if_data64) carries the
// byte counters at fixed offsets: ibytes at 96, obytes at 104.
func darwinInterfaceCounters() ([]netCounter, error) {
	const (
		hdrLen    = 32 // sizeof(if_msghdr2) up to ifm_data
		ibytesOff = hdrLen + 64
		obytesOff = hdrLen + 72
		minLen    = obytesOff + 8
	)
	b, err := route.FetchRIB(unix.AF_UNSPEC, unix.NET_RT_IFLIST2, 0)
	if err != nil {
		return nil, err
	}
	names := map[int]string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		names[iface.Index] = iface.Name
	}
	var out []netCounter
	for i := 0; i+4 <= len(b); {
		msgLen := int(binary.LittleEndian.Uint16(b[i:]))
		if msgLen <= 0 || i+msgLen > len(b) {
			break
		}
		if b[i+3] == unix.RTM_IFINFO2 && msgLen >= minLen {
			index := int(binary.LittleEndian.Uint16(b[i+12:]))
			if name := names[index]; name != "" {
				out = append(out, netCounter{
					name:    name,
					rxBytes: binary.LittleEndian.Uint64(b[i+ibytesOff:]),
					txBytes: binary.LittleEndian.Uint64(b[i+obytesOff:]),
				})
			}
		}
		i += msgLen
	}
	if len(out) == 0 {
		return nil, errors.New("no interface counters in the NET_RT_IFLIST2 dump")
	}
	return out, nil
}

// sysctlInt reads a 32-bit sysctl, returning -1 when it is absent.
func sysctlInt(name string) int {
	b, err := unix.SysctlRaw(name)
	if err != nil {
		return -1
	}
	switch len(b) {
	case 4:
		return int(int32(binary.LittleEndian.Uint32(b)))
	case 8:
		return int(int64(binary.LittleEndian.Uint64(b)))
	}
	return -1
}
