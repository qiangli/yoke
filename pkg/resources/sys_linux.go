//go:build linux

package resources

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// procRoot is overridable so the sysfs/procfs readers stay testable.
var procRoot = "/proc"

var sysRoot = "/sys"

func sampleCounters() (counters, error) {
	c := counters{at: time.Now()}
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return c, err
	}
	agg, cores, ok := parseProcStat(f)
	f.Close()
	c.cpu, c.perCore, c.hasCPU = agg, cores, ok

	if f, err := os.Open(filepath.Join(procRoot, "net", "dev")); err == nil {
		c.net = parseNetDev(f)
		f.Close()
	} else {
		c.warnings = append(c.warnings, "network: "+err.Error())
	}
	if f, err := os.Open(filepath.Join(procRoot, "diskstats")); err == nil {
		c.diskIO = parseDiskstats(f)
		f.Close()
	}
	return c, nil
}

func cpuStatic() (string, int, int) {
	model := ""
	if f, err := os.Open(filepath.Join(procRoot, "cpuinfo")); err == nil {
		model = parseCPUModel(f)
		f.Close()
	}
	// Physical core count is the distinct (package, core) pairs; deriving it
	// from topology sysfs is cheaper and more reliable than re-parsing
	// cpuinfo's per-cpu blocks.
	physical := 0
	if entries, err := filepath.Glob(filepath.Join(sysRoot, "devices/system/cpu/cpu[0-9]*/topology/core_id")); err == nil {
		seen := map[string]bool{}
		for _, e := range entries {
			core, err1 := os.ReadFile(e)
			pkg, err2 := os.ReadFile(filepath.Join(filepath.Dir(e), "physical_package_id"))
			if err1 != nil {
				continue
			}
			if err2 != nil {
				pkg = nil
			}
			seen[string(pkg)+"/"+string(core)] = true
		}
		physical = len(seen)
	}
	return model, runtime.NumCPU(), physical
}

func loadAverage() ([]float64, bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, "loadavg"))
	if err != nil {
		return nil, false
	}
	return parseLoadavg(string(b))
}

func memoryStats() (Memory, error) {
	f, err := os.Open(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return Memory{}, err
	}
	defer f.Close()
	info := parseMeminfo(f)
	m := Memory{
		TotalBytes:     info["MemTotal"],
		AvailableBytes: info["MemAvailable"],
		SwapTotalBytes: info["SwapTotal"],
		Source:         "/proc/meminfo",
	}
	if m.AvailableBytes == 0 {
		// Pre-3.14 kernels: the classic free+buffers+cached estimate.
		m.AvailableBytes = info["MemFree"] + info["Buffers"] + info["Cached"]
	}
	if m.TotalBytes > m.AvailableBytes {
		m.UsedBytes = m.TotalBytes - m.AvailableBytes
	}
	if m.SwapTotalBytes > info["SwapFree"] {
		m.SwapUsedBytes = m.SwapTotalBytes - info["SwapFree"]
	}
	return m, nil
}

func diskStats() ([]Disk, error) {
	f, err := os.Open(filepath.Join(procRoot, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Disk
	for _, m := range parseMountinfo(f) {
		var st unix.Statfs_t
		if err := unix.Statfs(m.point, &st); err != nil {
			continue
		}
		d := diskFromStatfs(m.point, m.device, m.fstype, uint64(st.Bsize), st.Blocks, st.Bfree, st.Bavail)
		if d.TotalBytes == 0 {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// gpuStats reads the kernel's own GPU nodes: the NVIDIA driver's procfs
// entries and the DRM sysfs tree (amdgpu/i915). Nothing is executed —
// nvidia-smi and lspci are deliberately not consulted.
func gpuStats() ([]GPU, error) {
	var out []GPU
	out = append(out, nvidiaProcGPUs()...)
	if len(out) == 0 {
		out = append(out, drmGPUs()...)
	}
	return out, nil
}

func nvidiaProcGPUs() []GPU {
	dirs, err := filepath.Glob(filepath.Join(procRoot, "driver/nvidia/gpus/*"))
	if err != nil {
		return nil
	}
	var out []GPU
	for _, dir := range dirs {
		g := GPU{Vendor: "NVIDIA", Source: "/proc/driver/nvidia"}
		if b, err := os.ReadFile(filepath.Join(dir, "information")); err == nil {
			for line := range strings.SplitSeq(string(b), "\n") {
				if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "Model" {
					g.Name = strings.TrimSpace(value)
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(dir, "fb_memory_usage")); err == nil {
			for line := range strings.SplitSeq(string(b), "\n") {
				key, value, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				bytes, ok := parseMiB(value)
				if !ok {
					continue
				}
				switch strings.TrimSpace(key) {
				case "Total":
					g.VRAMBytes = bytes
				case "Used":
					g.VRAMUsedBytes = bytes
				}
			}
		}
		if g.Name == "" {
			g.Name = "NVIDIA GPU"
		}
		g.VRAMKind = "dedicated"
		out = append(out, g)
	}
	return out
}

// parseMiB reads nvidia's "12288 MiB" value shape.
func parseMiB(s string) (uint64, bool) {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	switch strings.ToLower(fields[1]) {
	case "mib", "mb":
		return v * 1024 * 1024, true
	case "kib", "kb":
		return v * 1024, true
	case "gib", "gb":
		return v * 1024 * 1024 * 1024, true
	}
	return 0, false
}

func drmGPUs() []GPU {
	cards, err := filepath.Glob(filepath.Join(sysRoot, "class/drm/card[0-9]"))
	if err != nil {
		return nil
	}
	var out []GPU
	for _, card := range cards {
		dev := filepath.Join(card, "device")
		g := GPU{Name: filepath.Base(card), Source: "/sys/class/drm"}
		if b, err := os.ReadFile(filepath.Join(dev, "vendor")); err == nil {
			g.Vendor = pciVendor(strings.TrimSpace(string(b)))
		}
		if b, err := os.ReadFile(filepath.Join(dev, "mem_info_vram_total")); err == nil {
			if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
				g.VRAMBytes, g.VRAMKind = v, "dedicated"
			}
		}
		if b, err := os.ReadFile(filepath.Join(dev, "mem_info_vram_used")); err == nil {
			if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
				g.VRAMUsedBytes = v
			}
		}
		if b, err := os.ReadFile(filepath.Join(dev, "gpu_busy_percent")); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
				g.UtilizationPercent = v
			}
		}
		out = append(out, g)
	}
	return out
}

func pciVendor(id string) string {
	switch strings.ToLower(id) {
	case "0x10de":
		return "NVIDIA"
	case "0x1002":
		return "AMD"
	case "0x8086":
		return "Intel"
	}
	return id
}
