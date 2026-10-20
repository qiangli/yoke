package resources

import (
	"fmt"
	"time"
)

// metric names the watcher evaluates, one per reading.
const (
	metricDiskUsed      = "disk-used"
	metricDiskFree      = "disk-free"
	metricMemUsed       = "mem-used"
	metricSwapUsed      = "swap-used"
	metricCPUUsed       = "cpu-used"
	metricGPUUtil       = "gpu-util"
	metricGPUMem        = "gpu-mem"
	metricNetThroughput = "net-throughput"
	metricNetErrors     = "net-errors"
	metricNetLink       = "net-link"
)

// reading is one metric observation extracted from a System sample: the
// value, the unit it is compared in, the thresholds that classify it and
// whether the threshold only counts after it has held for Sustain.
type reading struct {
	resource string
	metric   string
	value    float64
	unit     string
	warn     float64
	crit     float64
	sustain  time.Duration
	detail   string
	// immediate transitions bypass the count hysteresis (link down).
	immediate bool
	// ifaceUp carries the link state for the net-link metric.
	ifaceUp bool
	// invert marks floor-style metrics (disk-free): lower is worse.
	invert bool
	// hasValue is false when the platform cannot supply the metric at all.
	hasValue bool
}

type netSample struct {
	at                   time.Time
	rxBytes, txBytes     uint64
	rxPackets, txPackets uint64
	rxErrors, txErrors   uint64
}

// extractReadings turns one System sample into the metric readings the
// watcher evaluates. cfgFor resolves the effective WatchConfig per resource
// name (per-resource override precedence is the caller's resolution).
func extractReadings(sys *System, cfgFor func(resource string) WatchConfig) []reading {
	prefix := WatchNamePrefix(sys.Host)
	var out []reading
	cfgOf := func(resource string) WatchConfig { return cfgFor(resource) }

	// CPU: the platform's own utilization figure (ticks, or loadavg/cores on
	// darwin). Sustained by default: a busy minute proves nothing.
	if sys.CPU.UsagePercent > 0 || sys.CPU.Source == "loadavg" || sys.CPU.Source == "ticks" {
		r := prefix + ".cpu"
		cfg := cfgOf(r)
		if cfg.Enabled {
			out = append(out, reading{
				resource: r, metric: metricCPUUsed,
				value: sys.CPU.UsagePercent, unit: "percent",
				warn: cfg.CPUWarn, crit: cfg.CPUCrit, sustain: cfg.CPUSustain,
				detail:   fmt.Sprintf("cpu %.1f%% (%s)", sys.CPU.UsagePercent, sys.CPU.Source),
				hasValue: sys.CPU.Source != "unavailable" && sys.CPU.UsagePercent > 0,
			})
		}
	}

	// Memory and swap.
	if sys.Memory.TotalBytes > 0 {
		r := prefix + ".mem"
		cfg := cfgOf(r)
		if cfg.Enabled {
			usedPct := sys.Memory.UsedPercent
			if usedPct == 0 && sys.Memory.TotalBytes > 0 {
				usedPct = pct(sys.Memory.UsedBytes, sys.Memory.TotalBytes)
			}
			out = append(out, reading{
				resource: r, metric: metricMemUsed,
				value: usedPct, unit: "percent",
				warn: cfg.MemWarn, crit: cfg.MemCrit,
				detail:   fmt.Sprintf("memory %.1f%% used", usedPct),
				hasValue: usedPct > 0,
			})
			if sys.Memory.SwapTotalBytes > 0 {
				swapPct := pct(sys.Memory.SwapUsedBytes, sys.Memory.SwapTotalBytes)
				out = append(out, reading{
					resource: r, metric: metricSwapUsed,
					value: swapPct, unit: "percent",
					warn: cfg.SwapWarn, crit: cfg.SwapCrit,
					detail:   fmt.Sprintf("swap %.1f%% used", swapPct),
					hasValue: true,
				})
			}
		}
	}

	// Disks: used percent and absolute free bytes, whichever trips first.
	for _, d := range sys.Disks {
		r := prefix + ".disk-" + watchSlug(d.Mount)
		cfg := cfgOf(r)
		if !cfg.Enabled {
			continue
		}
		usedPct := d.UsedPercent
		if usedPct == 0 && d.TotalBytes > 0 {
			usedPct = pct(d.UsedBytes, d.TotalBytes)
		}
		out = append(out, reading{
			resource: r, metric: metricDiskUsed,
			value: usedPct, unit: "percent",
			warn: cfg.DiskWarnUsed, crit: cfg.DiskCritUsed,
			detail:   fmt.Sprintf("%s %.1f%% used (%s free)", d.Mount, usedPct, HumanBytes(d.FreeBytes)),
			hasValue: usedPct > 0,
		})
		// The byte floor skips mounts smaller than the floor itself, the
		// same rule CheckDiskFloor applies (boot/firmware partitions).
		if cfg.DiskFreeWarnBytes > 0 && d.TotalBytes >= cfg.DiskFreeWarnBytes {
			out = append(out, reading{
				resource: r, metric: metricDiskFree,
				value: float64(d.FreeBytes), unit: "bytes",
				warn: float64(cfg.DiskFreeWarnBytes), crit: float64(cfg.DiskFreeCritBytes),
				invert:   true,
				detail:   fmt.Sprintf("%s %s free", d.Mount, HumanBytes(d.FreeBytes)),
				hasValue: true,
			})
		}
	}

	// GPUs: utilization and (dedicated or unified) video memory. Platforms
	// that cannot supply a figure report zero; zero is treated as absent,
	// never as an idle reading.
	for _, g := range sys.GPUs {
		r := prefix + ".gpu-" + watchSlug(g.Name)
		cfg := cfgOf(r)
		if !cfg.Enabled {
			continue
		}
		if g.UtilizationPercent > 0 {
			out = append(out, reading{
				resource: r, metric: metricGPUUtil,
				value: g.UtilizationPercent, unit: "percent",
				warn: cfg.GPUWarn, crit: cfg.GPUCrit, sustain: cfg.GPUSustain,
				detail:   fmt.Sprintf("%s util %.1f%%", g.Name, g.UtilizationPercent),
				hasValue: true,
			})
		}
		if g.VRAMBytes > 0 && g.VRAMUsedBytes > 0 {
			memPct := pct(g.VRAMUsedBytes, g.VRAMBytes)
			out = append(out, reading{
				resource: r, metric: metricGPUMem,
				value: memPct, unit: "percent",
				warn: cfg.GPUMemWarn, crit: cfg.GPUMemCrit,
				detail:   fmt.Sprintf("%s vram %.1f%% used", g.Name, memPct),
				hasValue: true,
			})
		}
	}
	return out
}

// netReadings derives rate- and delta-based interface metrics (throughput,
// errors, link state) from the current sample plus the previous cumulative
// counters the watcher retained.
func netReadings(sys *System, prev map[string]netSample, at time.Time, cfgFor func(resource string) WatchConfig) ([]reading, map[string]netSample) {
	prefix := WatchNamePrefix(sys.Host)
	var out []reading
	cur := make(map[string]netSample, len(sys.Network))
	for _, n := range sys.Network {
		r := prefix + ".net-" + watchSlug(n.Name)
		cfg := cfgFor(r)
		cur[n.Name] = netSample{at: at, rxBytes: n.RxBytes, txBytes: n.TxBytes,
			rxPackets: n.RxPackets, txPackets: n.TxPackets, rxErrors: n.RxErrors, txErrors: n.TxErrors}
		if !cfg.Enabled {
			continue
		}
		out = append(out, reading{
			resource: r, metric: metricNetLink,
			value: boolTo01(n.Up), unit: "state",
			warn: 0, crit: 0, immediate: true, ifaceUp: n.Up,
			detail: n.Name, hasValue: true,
		})
		p, ok := prev[n.Name]
		if !ok {
			continue
		}
		elapsed := at.Sub(p.at).Seconds()
		if elapsed <= 0 {
			continue
		}
		dPkt := deltaGE(p.rxPackets, n.RxPackets) + deltaGE(p.txPackets, n.TxPackets)
		dErr := deltaGE(p.rxErrors, n.RxErrors) + deltaGE(p.txErrors, n.TxErrors)
		if dPkt > 0 && dErr > 0 {
			errPct := float64(dErr) / float64(dPkt) * 100
			out = append(out, reading{
				resource: r, metric: metricNetErrors,
				value: errPct, unit: "percent",
				warn: cfg.NetErrWarn, crit: cfg.NetErrCrit,
				detail:   fmt.Sprintf("%s %.2f%% of packets errored/dropped", n.Name, errPct),
				hasValue: true,
			})
		}
		link := cfg.LinkMbps
		if link <= 0 && n.LinkMbps > 0 {
			link = float64(n.LinkMbps)
		}
		if link > 0 {
			combined := deltaGE(p.rxBytes, n.RxBytes) + deltaGE(p.txBytes, n.TxBytes)
			mbps := float64(combined) * 8 / elapsed / 1e6
			pctOfLink := mbps / link * 100
			out = append(out, reading{
				resource: r, metric: metricNetThroughput,
				value: pctOfLink, unit: "percent",
				warn: cfg.NetWarn, crit: cfg.NetCrit, sustain: cfg.NetSustain,
				detail:   fmt.Sprintf("%s %.1f Mbps of %.0f Mbps link", n.Name, mbps, link),
				hasValue: true,
			})
		}
	}
	return out, cur
}

func deltaGE(prev, cur uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

func boolTo01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
