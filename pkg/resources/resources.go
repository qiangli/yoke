// Package resources reports live SYSTEM-LEVEL resource utilization — CPU,
// memory, disk, network, and (best effort) GPU — so a steward can judge
// whether the host itself is the bottleneck behind a slow fleet.
//
// Two rules shape the implementation:
//
//   - Pure Go, no cgo. Every number comes from an OS interface read
//     directly: /proc and /sys on Linux, sysctl (+ the routing socket's
//     interface list) on darwin, and the kernel32/ntdll/iphlpapi entry
//     points via golang.org/x/sys/windows on Windows. No vendored cgo
//     telemetry library, and — per the repo contract — no shelling out to
//     system_profiler/nvidia-smi/wmic to implement our own behavior. A
//     metric a platform cannot supply purely is omitted with a warning,
//     never approximated silently.
//   - Sample-and-diff. Utilization is a rate, not a level: Collect takes
//     two counter samples separated by Options.Interval and reports the
//     delta. With Interval == 0 (the board panel's cheap path) the CPU
//     figure degrades to the cumulative since-boot ratio and byte rates
//     are omitted rather than invented; every reading carries the Source
//     that produced it and an explicit At timestamp.
//
// Platform honesty, stated once so callers can reason about the envelope:
// Linux supplies per-core ticks, load average, and disk IO; darwin has no
// per-core tick sysctl (that lives behind mach host_processor_info, which
// needs cgo), so its CPU figure is load-average-derived and per-core is
// omitted; Windows supplies aggregate and per-core ticks but no disk IO
// here. GPU absence is normal and is reported as such, not as an error.
package resources

import (
	"context"
	"os"
	"runtime"
	"sort"
	"time"
)

// SchemaVersion is the versioned envelope tag for the `resources` JSON
// envelopes (`resources system` and `resources fleet`).
const SchemaVersion = "bashy-resources-v1"

// DefaultInterval is the gap between the two counter samples. Long enough
// that a tick counter moves on an idle machine, short enough that a human
// running the command does not notice the wait.
const DefaultInterval = 300 * time.Millisecond

// Options controls a collection pass.
type Options struct {
	// Interval is the gap between the two counter samples. Zero means a
	// single sample: CPU falls back to the since-boot ratio and rates are
	// omitted.
	Interval time.Duration
	// Now overrides the reading timestamp (tests).
	Now time.Time
}

// System is the bashy-resources-v1 envelope.
type System struct {
	SchemaVersion string      `json:"schema_version"`
	At            time.Time   `json:"at"`
	Host          string      `json:"host,omitempty"`
	OS            string      `json:"os"`
	Arch          string      `json:"arch"`
	SampleSeconds float64     `json:"sample_seconds"`
	CPU           CPU         `json:"cpu"`
	Memory        Memory      `json:"memory"`
	Disks         []Disk      `json:"disks"`
	Network       []Interface `json:"network"`
	GPUs          []GPU       `json:"gpus"`
	Warnings      []string    `json:"warnings,omitempty"`
}

// CPU is aggregate plus per-core utilization over the sample window.
type CPU struct {
	Model          string    `json:"model,omitempty"`
	LogicalCores   int       `json:"logical_cores"`
	PhysicalCores  int       `json:"physical_cores,omitempty"`
	UsagePercent   float64   `json:"usage_percent"`
	PerCorePercent []float64 `json:"per_core_percent,omitempty"`
	LoadAverage    []float64 `json:"load_average,omitempty"`
	Pressure       string    `json:"pressure"`
	// Source names the derivation: "ticks" (sampled delta), "ticks-boot"
	// (cumulative since boot — a single sample), or "loadavg" (darwin,
	// where per-core ticks need cgo).
	Source string `json:"source"`
}

// Memory is physical memory plus swap. Available is the platform's own
// notion of "memory a new allocation can have" (MemAvailable on Linux,
// kern.memorystatus_level on darwin, ullAvailPhys on Windows) rather than
// a free-page count, which reads as alarmingly low on every modern OS.
type Memory struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
	SwapTotalBytes uint64  `json:"swap_total_bytes,omitempty"`
	SwapUsedBytes  uint64  `json:"swap_used_bytes,omitempty"`
	Pressure       string  `json:"pressure"`
	Source         string  `json:"source,omitempty"`
}

// Disk is one mounted filesystem. The IO rates are best effort (Linux
// /proc/diskstats today) and are omitted where the platform or the sample
// window cannot supply them.
type Disk struct {
	Mount            string  `json:"mount"`
	Device           string  `json:"device,omitempty"`
	FSType           string  `json:"fstype,omitempty"`
	TotalBytes       uint64  `json:"total_bytes"`
	UsedBytes        uint64  `json:"used_bytes"`
	FreeBytes        uint64  `json:"free_bytes"`
	UsedPercent      float64 `json:"used_percent"`
	ReadBytesPerSec  float64 `json:"read_bytes_per_sec,omitempty"`
	WriteBytesPerSec float64 `json:"write_bytes_per_sec,omitempty"`
}

// Interface is one network interface: cumulative counters plus the rate
// observed over the sample window.
type Interface struct {
	Name          string  `json:"name"`
	RxBytes       uint64  `json:"rx_bytes"`
	TxBytes       uint64  `json:"tx_bytes"`
	RxBytesPerSec float64 `json:"rx_bytes_per_sec,omitempty"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec,omitempty"`
}

// GPU is a detected graphics processor. Absent GPUs are normal: the slice
// is empty and the renderer prints "none".
type GPU struct {
	Vendor string `json:"vendor,omitempty"`
	Name   string `json:"name"`
	// VRAMBytes is dedicated video memory, or the unified pool on systems
	// where the GPU shares system memory (VRAMKind says which).
	VRAMBytes          uint64  `json:"vram_bytes,omitempty"`
	VRAMUsedBytes      uint64  `json:"vram_used_bytes,omitempty"`
	VRAMKind           string  `json:"vram_kind,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	Source             string  `json:"source,omitempty"`
}

// counters is one raw sample of the rate-bearing kernel counters.
type counters struct {
	at        time.Time
	cpu       cpuTicks
	perCore   []cpuTicks
	hasCPU    bool
	net       []netCounter
	diskIO    []ioCounter
	warnings  []string
	unsampled bool // platform supplies no counters at all
}

// cpuTicks is a monotonically increasing pair: all scheduler time and the
// idle share of it, in whatever unit the platform counts.
type cpuTicks struct{ total, idle uint64 }

type netCounter struct {
	name    string
	rxBytes uint64
	txBytes uint64
}

type ioCounter struct {
	device     string
	readBytes  uint64
	writeBytes uint64
}

// Collect takes the reading. It never fails because one subsystem is
// unreadable — that subsystem lands in Warnings and the rest is reported.
func Collect(ctx context.Context, opts Options) (*System, error) {
	host, _ := os.Hostname()
	sys := &System{SchemaVersion: SchemaVersion, Host: host, OS: runtime.GOOS, Arch: runtime.GOARCH}

	first, err := sampleCounters()
	if err != nil {
		sys.warn("counters: " + err.Error())
	}
	second := first
	if opts.Interval > 0 && !first.unsampled {
		if err := sleepCtx(ctx, opts.Interval); err != nil {
			return nil, err
		}
		if s, err := sampleCounters(); err != nil {
			sys.warn("counters: " + err.Error())
		} else {
			second = s
		}
	}
	sys.Warnings = append(sys.Warnings, second.warnings...)

	elapsed := second.at.Sub(first.at).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	sys.SampleSeconds = round(elapsed, 3)
	sys.At = second.at.UTC()
	if !opts.Now.IsZero() {
		sys.At = opts.Now.UTC()
	}

	sys.CPU = buildCPU(first, second)
	if mem, err := memoryStats(); err != nil {
		sys.warn("memory: " + err.Error())
	} else {
		mem.UsedPercent = pct(mem.UsedBytes, mem.TotalBytes)
		mem.Pressure = classify(mem.UsedPercent)
		sys.Memory = mem
	}
	if disks, err := diskStats(); err != nil {
		sys.warn("disk: " + err.Error())
	} else {
		sys.Disks = applyDiskIO(dedupeByKey(disks, byDevice), first.diskIO, second.diskIO, elapsed)
	}
	sys.Network = buildNetwork(first.net, second.net, elapsed)
	if gpus, err := gpuStats(); err != nil {
		sys.warn("gpu: " + err.Error())
	} else {
		sys.GPUs = gpus
	}
	return sys, nil
}

func (s *System) warn(msg string) { s.Warnings = append(s.Warnings, msg) }

func buildCPU(first, second counters) CPU {
	model, logical, physical := cpuStatic()
	c := CPU{Model: model, LogicalCores: logical, PhysicalCores: physical}
	if load, ok := loadAverage(); ok {
		c.LoadAverage = load
	}
	switch {
	case second.hasCPU && second.cpu.total > first.cpu.total:
		c.UsagePercent = tickUsage(first.cpu, second.cpu)
		c.Source = "ticks"
		for i := range second.perCore {
			prev := cpuTicks{}
			if i < len(first.perCore) {
				prev = first.perCore[i]
			}
			c.PerCorePercent = append(c.PerCorePercent, tickUsage(prev, second.perCore[i]))
		}
	case second.hasCPU:
		// A single sample (or a window too short for the tick clock to
		// move): the since-boot ratio is still a true statement about the
		// host, so report it under a Source that says so.
		c.UsagePercent = tickUsage(cpuTicks{}, second.cpu)
		c.Source = "ticks-boot"
		for _, core := range second.perCore {
			c.PerCorePercent = append(c.PerCorePercent, tickUsage(cpuTicks{}, core))
		}
	case len(c.LoadAverage) > 0 && logical > 0:
		// Runnable threads per core: on darwin this is the only
		// utilization signal reachable without cgo.
		c.UsagePercent = round(min(c.LoadAverage[0]/float64(logical)*100, 100), 1)
		c.Source = "loadavg"
	default:
		c.Source = "unavailable"
	}
	c.Pressure = classify(c.UsagePercent)
	return c
}

func tickUsage(prev, cur cpuTicks) float64 {
	total := cur.total - prev.total
	idle := cur.idle - prev.idle
	if cur.total < prev.total || cur.idle < prev.idle || total == 0 {
		return 0
	}
	if idle > total {
		idle = total
	}
	return round(float64(total-idle)/float64(total)*100, 1)
}

func buildNetwork(first, second []netCounter, elapsed float64) []Interface {
	prev := map[string]netCounter{}
	for _, n := range first {
		prev[n.name] = n
	}
	out := make([]Interface, 0, len(second))
	for _, n := range second {
		iface := Interface{Name: n.name, RxBytes: n.rxBytes, TxBytes: n.txBytes}
		if p, ok := prev[n.name]; ok && elapsed > 0 {
			iface.RxBytesPerSec = rate(p.rxBytes, n.rxBytes, elapsed)
			iface.TxBytesPerSec = rate(p.txBytes, n.txBytes, elapsed)
		}
		out = append(out, iface)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func applyDiskIO(disks []Disk, first, second []ioCounter, elapsed float64) []Disk {
	if elapsed <= 0 || len(second) == 0 {
		return disks
	}
	prev := map[string]ioCounter{}
	for _, c := range first {
		prev[c.device] = c
	}
	cur := map[string]ioCounter{}
	for _, c := range second {
		cur[c.device] = c
	}
	for i := range disks {
		name := deviceKey(disks[i].Device)
		c, ok := cur[name]
		if !ok {
			continue
		}
		p, ok := prev[name]
		if !ok {
			continue
		}
		disks[i].ReadBytesPerSec = rate(p.readBytes, c.readBytes, elapsed)
		disks[i].WriteBytesPerSec = rate(p.writeBytes, c.writeBytes, elapsed)
	}
	return disks
}

func rate(prev, cur uint64, elapsed float64) float64 {
	if cur < prev || elapsed <= 0 {
		return 0
	}
	return round(float64(cur-prev)/elapsed, 1)
}

// classify maps a utilization percentage onto the three-level pressure
// vocabulary the board panel and the steward read.
func classify(p float64) string {
	switch {
	case p >= 90:
		return "critical"
	case p >= 75:
		return "elevated"
	default:
		return "normal"
	}
}

func pct(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}
	return round(float64(part)/float64(whole)*100, 1)
}

func round(v float64, places int) float64 {
	f := 1.0
	for range places {
		f *= 10
	}
	r := float64(int64(v*f + 0.5))
	if v < 0 {
		r = float64(int64(v*f - 0.5))
	}
	return r / f
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
