package resources

import (
	"strings"
	"testing"
	"time"
)

// The watch evaluation gate (S480): these tests define the contract for
// threshold evaluation, sustained windows, hysteresis, transitions and
// re-notification. RED FIRST: they were written before the implementation.

const gib = 1 << 30

func fakeDisk(mount string, usedPct float64, totalBytes uint64) Disk {
	used := uint64(float64(totalBytes) * usedPct / 100)
	return Disk{
		Mount:       mount,
		Device:      "/dev/disk0s1",
		FSType:      "apfs",
		TotalBytes:  totalBytes,
		UsedBytes:   used,
		FreeBytes:   totalBytes - used,
		UsedPercent: usedPct,
	}
}

func fakeSystem() *System {
	return &System{
		SchemaVersion: SchemaVersion,
		Host:          "host-a.example",
		At:            time.Now().UTC(),
		OS:            "darwin",
		CPU:           CPU{UsagePercent: 3, LogicalCores: 8, Source: "ticks"},
		Memory: Memory{
			TotalBytes:     64 * gib,
			UsedBytes:      19 * gib,
			AvailableBytes: 45 * gib,
			UsedPercent:    30,
			SwapTotalBytes: 8 * gib,
			SwapUsedBytes:  1 * gib,
		},
		Disks:   []Disk{fakeDisk("/data", 70, 500*gib)},
		Network: []Interface{{Name: "en0", RxBytes: 1000, TxBytes: 1000, Up: true}},
	}
}

// --- spec defaults ----------------------------------------------------------

func TestWatchDefaultThresholds(t *testing.T) {
	cfg := DefaultWatchConfig()
	checks := []struct {
		name string
		got  any
		want any
	}{
		{"disk warn used", cfg.DiskWarnUsed, 85.0},
		{"disk crit used", cfg.DiskCritUsed, 95.0},
		{"disk free warn", cfg.DiskFreeWarnBytes, uint64(20 * gib)},
		{"disk free crit", cfg.DiskFreeCritBytes, uint64(10 * gib)},
		{"mem warn", cfg.MemWarn, 85.0},
		{"mem crit", cfg.MemCrit, 95.0},
		{"swap warn", cfg.SwapWarn, 50.0},
		{"swap crit", cfg.SwapCrit, 80.0},
		{"cpu warn", cfg.CPUWarn, 85.0},
		{"cpu crit", cfg.CPUCrit, 95.0},
		{"cpu sustain", cfg.CPUSustain, 5 * time.Minute},
		{"gpu warn", cfg.GPUWarn, 90.0},
		{"gpu crit", cfg.GPUCrit, 98.0},
		{"gpu sustain", cfg.GPUSustain, 5 * time.Minute},
		{"gpu mem warn", cfg.GPUMemWarn, 90.0},
		{"gpu mem crit", cfg.GPUMemCrit, 97.0},
		{"net warn", cfg.NetWarn, 80.0},
		{"net crit", cfg.NetCrit, 95.0},
		{"net sustain", cfg.NetSustain, 5 * time.Minute},
		{"net err warn", cfg.NetErrWarn, 1.0},
		{"hysteresis", cfg.Hysteresis, 3},
		{"renotify", cfg.Renotify, 30 * time.Minute},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestClassifyLevel(t *testing.T) {
	cases := []struct {
		v         float64
		warn, cri float64
		want      Level
	}{
		{84.9, 85, 95, LevelNormal},
		{85, 85, 95, LevelWarning},
		{86, 85, 95, LevelWarning},
		{94.9, 85, 95, LevelWarning},
		{95, 85, 95, LevelCritical},
		{100, 85, 95, LevelCritical},
		{0, 85, 95, LevelNormal},
	}
	for _, c := range cases {
		if got := ClassifyLevel(c.v, c.warn, c.cri); got != c.want {
			t.Errorf("ClassifyLevel(%v, %v, %v) = %v, want %v", c.v, c.warn, c.cri, got, c.want)
		}
	}
}

// --- disk: used percent and absolute free floor, whichever trips first -------

func TestEvaluateDiskUsedPercent(t *testing.T) {
	w := NewWatcher(DefaultWatchConfig())
	base := fakeSystem()
	now := time.Now().UTC()

	// 86% used, plenty free: warning on the used-percent rule.
	sys := fakeSystem()
	sys.Disks = []Disk{fakeDisk("/data", 86, 500*gib)}
	for range 3 {
		w.Observe(now, sys)
	}
	evs := eventsFor(w, "host-a.example.disk-data", "disk-used")
	if len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("86%% used: got %v, want one warning transition", summarize(evs))
	}
	if evs[0].Threshold != 85 {
		t.Errorf("threshold = %v, want 85 (warn used pct)", evs[0].Threshold)
	}

	// 96% used: critical.
	w2 := NewWatcher(DefaultWatchConfig())
	sys2 := fakeSystem()
	sys2.Disks = []Disk{fakeDisk("/data", 96, 500*gib)}
	for range 3 {
		w2.Observe(now, sys2)
	}
	evs = eventsFor(w2, "host-a.example.disk-data", "disk-used")
	if len(evs) != 1 || evs[0].Level != LevelCritical {
		t.Fatalf("96%% used: got %v, want one critical transition", summarize(evs))
	}
	_ = base
}

func TestEvaluateDiskFreeFloor(t *testing.T) {
	now := time.Now().UTC()
	// 12% used on a 200 GiB disk leaves 176 GiB: no floor alarm. A 20 GiB
	// total would leave 17.6 GiB free: warning via the 20 GiB floor even
	// though used percent is far below 85.
	cfg := DefaultWatchConfig()
	sys := fakeSystem()
	sys.Disks = []Disk{fakeDisk("/data", 12, 20*gib)}
	w := NewWatcher(cfg)
	for range 3 {
		w.Observe(now, sys)
	}
	evs := eventsFor(w, "host-a.example.disk-data", "disk-free")
	if len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("17.6GiB free: got %v, want one disk-free warning", summarize(evs))
	}
	if evs[0].Threshold != float64(20*gib) {
		t.Errorf("disk-free threshold = %v, want 20GiB", evs[0].Threshold)
	}

	// 9 GiB free on a big disk: critical via the 10 GiB floor at only 5% used.
	w2 := NewWatcher(DefaultWatchConfig())
	sys2 := fakeSystem()
	sys2.Disks = []Disk{fakeDisk("/data", 95.5, 200*gib)} // 9 GiB free
	for range 3 {
		w2.Observe(now, sys2)
	}
	evs = eventsFor(w2, "host-a.example.disk-data", "disk-free")
	if len(evs) != 1 || evs[0].Level != LevelCritical {
		t.Fatalf("9GiB free: got %v, want one disk-free critical", summarize(evs))
	}

	// A mount smaller than the floor itself never trips the byte floor
	// (boot/firmware partitions), matching CheckDiskFloor's rule.
	w3 := NewWatcher(DefaultWatchConfig())
	sys3 := fakeSystem()
	sys3.Disks = []Disk{fakeDisk("/boot", 99, 1*gib)}
	for range 3 {
		w3.Observe(now, sys3)
	}
	if evs := eventsFor(w3, "host-a.example.disk-boot", "disk-free"); len(evs) != 0 {
		t.Fatalf("1GiB mount tripped the byte floor: %v", summarize(evs))
	}
}

// --- memory and swap --------------------------------------------------------

func TestEvaluateMemoryAndSwap(t *testing.T) {
	now := time.Now().UTC()
	sys := fakeSystem()
	sys.Memory = Memory{TotalBytes: 100 * gib, UsedBytes: 86 * gib, UsedPercent: 86,
		SwapTotalBytes: 10 * gib, SwapUsedBytes: 5.5 * gib} // swap 55%
	w := NewWatcher(DefaultWatchConfig())
	for range 3 {
		w.Observe(now, sys)
	}
	if evs := eventsFor(w, "host-a.example.mem", "mem-used"); len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("mem 86%%: %v", summarize(evs))
	}
	if evs := eventsFor(w, "host-a.example.mem", "swap-used"); len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("swap 55%%: %v", summarize(evs))
	}

	// No swap configured: the swap metric stays silent, not zero.
	w2 := NewWatcher(DefaultWatchConfig())
	sys2 := fakeSystem()
	sys2.Memory = Memory{TotalBytes: 100 * gib, UsedBytes: 96 * gib, UsedPercent: 96}
	for range 3 {
		w2.Observe(now, sys2)
	}
	if evs := eventsFor(w2, "host-a.example.mem", "swap-used"); len(evs) != 0 {
		t.Fatalf("swap with no total produced events: %v", summarize(evs))
	}
	if evs := eventsFor(w2, "host-a.example.mem", "mem-used"); len(evs) != 1 || evs[0].Level != LevelCritical {
		t.Fatalf("mem 96%%: %v", summarize(evs))
	}
}

// --- cpu: sustained window --------------------------------------------------

func TestEvaluateCPUSustained(t *testing.T) {
	cfg := DefaultWatchConfig()
	cfg.CPUSustain = 5 * time.Minute
	w := NewWatcher(cfg)
	start := time.Now().UTC()
	sys := fakeSystem()
	sys.CPU.UsagePercent = 90

	// Minutes 0..4 inside the window: still normal.
	for i := range 5 {
		w.Observe(start.Add(time.Duration(i)*time.Minute), sys)
	}
	if evs := allEvents(w, "host-a.example.cpu", "cpu-used"); len(evs) != 0 {
		t.Fatalf("sustained window not yet covered, but got %v", summarize(evs))
	}
	// Minute 5: the window is covered by points >= warn -> warning.
	w.Observe(start.Add(5*time.Minute), sys)
	evs := allEvents(w, "host-a.example.cpu", "cpu-used")
	if len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("sustained 90%% for 5m: %v", summarize(evs))
	}

	// A single spike inside a mostly-idle window never trips.
	w2 := NewWatcher(cfg)
	for i := range 6 {
		s := fakeSystem()
		s.CPU.UsagePercent = 5
		if i == 2 {
			s.CPU.UsagePercent = 99
		}
		w2.Observe(start.Add(time.Duration(i)*time.Minute), s)
	}
	if evs := allEvents(w2, "host-a.example.cpu", "cpu-used"); len(evs) != 0 {
		t.Fatalf("one-minute spike tripped the sustained alarm: %v", summarize(evs))
	}
}

// --- gpu ---------------------------------------------------------------------

func TestEvaluateGPU(t *testing.T) {
	now := time.Now().UTC()
	cfg := DefaultWatchConfig()
	cfg.GPUSustain = 0 // utilization immediate for this test
	sys := fakeSystem()
	sys.GPUs = []GPU{{Name: "gpu0", VRAMBytes: 100 * gib, VRAMUsedBytes: 91 * gib, UtilizationPercent: 99}}
	w := NewWatcher(cfg)
	for range 3 {
		w.Observe(now, sys)
	}
	if evs := eventsFor(w, "host-a.example.gpu-gpu0", "gpu-mem"); len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("gpu mem 91%%: %v", summarize(evs))
	}
	if evs := eventsFor(w, "host-a.example.gpu-gpu0", "gpu-util"); len(evs) != 1 || evs[0].Level != LevelCritical {
		t.Fatalf("gpu util 99%%: %v", summarize(evs))
	}
}

// --- network ------------------------------------------------------------------

func TestEvaluateNetErrors(t *testing.T) {
	start := time.Now().UTC()
	w := NewWatcher(DefaultWatchConfig())
	// Cumulative counters grow each minute; each delta carries 250 errors
	// over 12500 packets = 2%.
	for i := range 4 {
		at := start.Add(time.Duration(i) * time.Minute)
		sys := fakeSystem()
		sys.Network = []Interface{{Name: "en0", Up: true,
			RxBytes: uint64(i+1) * 1_000_000, TxBytes: uint64(i+1) * 500_000,
			RxPackets: uint64(1000 + i*7500), TxPackets: uint64(1000 + i*5000),
			RxErrors: uint64(i * 250), TxErrors: 0}}
		w.Observe(at, sys)
	}
	if evs := eventsFor(w, "host-a.example.net-en0", "net-errors"); len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("2%% interface errors: %v", summarize(evs))
	}
}

func TestEvaluateNetThroughputSustained(t *testing.T) {
	cfg := DefaultWatchConfig()
	w := NewWatcherWithConfigs(cfg, map[string]WatchConfig{
		"host-a.example.net-en0": OverrideOnly(cfg, map[string]string{"link-mbps": "100"}),
	})
	start := time.Now().UTC()
	// 90% of a 100 Mbps link is 11.25 MB/s combined, i.e. 337.5 MB per
	// minute per direction; counters grow cumulatively.
	for i := range 7 {
		at := start.Add(time.Duration(i) * time.Minute)
		sys := fakeSystem()
		mb := uint64(i) * 337_500_000
		sys.Network = []Interface{{Name: "en0", Up: true, RxBytes: mb, TxBytes: mb, LinkMbps: 100}}
		w.Observe(at, sys)
	}
	evs := allEvents(w, "host-a.example.net-en0", "net-throughput")
	if len(evs) != 1 || evs[0].Level != LevelWarning {
		t.Fatalf("sustained 90%% of link: %v", summarize(evs))
	}
	if evs[0].Unit != "percent" {
		t.Errorf("throughput unit = %q, want percent", evs[0].Unit)
	}
}

func TestEvaluateNetLinkDown(t *testing.T) {
	now := time.Now().UTC()
	w := NewWatcher(DefaultWatchConfig())
	up := fakeSystem()
	up.Network = []Interface{{Name: "en0", Up: true, RxBytes: 1, TxBytes: 1}}
	w.Observe(now, up)
	down := fakeSystem()
	down.Network = []Interface{{Name: "en0", Up: false, RxBytes: 1, TxBytes: 1}}
	evs := w.Observe(now.Add(time.Minute), down)
	found := false
	for _, e := range evs {
		if e.Resource == "host-a.example.net-en0" && e.Metric == "net-link" && e.Level == LevelCritical {
			found = true
		}
	}
	if !found {
		t.Fatalf("link down on a known-up interface must be critical immediately: %v", summarize(evs))
	}
}

// --- hysteresis, transitions, re-notify --------------------------------------

func TestWatcherHysteresisSuppressesFlap(t *testing.T) {
	w := NewWatcher(DefaultWatchConfig())
	start := time.Now().UTC()
	for i := range 8 {
		sys := fakeSystem()
		if i%2 == 0 {
			sys.Disks = []Disk{fakeDisk("/data", 86, 500*gib)}
		} else {
			sys.Disks = []Disk{fakeDisk("/data", 70, 500*gib)}
		}
		w.Observe(start.Add(time.Duration(i)*time.Minute), sys)
	}
	if evs := allEvents(w, "host-a.example.disk-data", "disk-used"); len(evs) != 0 {
		t.Fatalf("hovering at the threshold must not alarm: %v", summarize(evs))
	}
}

func TestWatcherTransitionsAndClear(t *testing.T) {
	w := NewWatcher(DefaultWatchConfig())
	start := time.Now().UTC()
	at := func(i int) time.Time { return start.Add(time.Duration(i) * time.Second) }

	// Three consecutive warnings commit the transition on the third.
	for i := range 3 {
		sys := fakeSystem()
		sys.Disks = []Disk{fakeDisk("/data", 86, 500*gib)}
		evs := w.Observe(at(i), sys)
		if i < 2 {
			for _, e := range evs {
				if e.Resource == "host-a.example.disk-data" && e.Metric == "disk-used" {
					t.Fatalf("alarm before hysteresis committed (sample %d): %v", i, e)
				}
			}
		} else {
			var got *Event
			for _, e := range evs {
				if e.Resource == "host-a.example.disk-data" && e.Metric == "disk-used" {
					got = &e
				}
			}
			if got == nil || got.Level != LevelWarning || got.From != LevelNormal {
				t.Fatalf("third sample must commit normal->warning: %v", summarize(evs))
			}
			if got.Host != "host-a.example" || got.At.IsZero() || got.Value != 86 {
				t.Fatalf("event is missing fields: %+v", got)
			}
		}
	}

	// Escalate to critical, then clear back to normal with a clear notice.
	for i, pct := range []float64{96, 96, 96, 70, 70, 70} {
		sys := fakeSystem()
		sys.Disks = []Disk{fakeDisk("/data", pct, 500*gib)}
		evs := w.Observe(at(10+i), sys)
		if pct == 70 && i == 5 {
			clear := false
			for _, e := range evs {
				if e.Resource == "host-a.example.disk-data" && e.Metric == "disk-used" && e.Level == LevelNormal && e.To == LevelNormal {
					clear = true
				}
			}
			if !clear {
				t.Fatalf("return to 70%% must emit a clear line: %v", summarize(evs))
			}
		}
	}
}

func TestWatcherRenotifyStillCritical(t *testing.T) {
	cfg := DefaultWatchConfig()
	cfg.Renotify = 30 * time.Minute
	w := NewWatcher(cfg)
	start := time.Now().UTC()
	sys := fakeSystem()
	sys.Disks = []Disk{fakeDisk("/data", 96, 500*gib)}
	for i := range 3 {
		w.Observe(start.Add(time.Duration(i)*time.Second), sys)
	}
	// 10 minutes later: still critical, no re-notify yet.
	evs := w.Observe(start.Add(10*time.Minute), sys)
	if countNotifies(evs, "host-a.example.disk-data", "disk-used") != 0 {
		t.Fatalf("re-notified before the interval: %v", summarize(evs))
	}
	// 31 minutes after the transition: exactly one re-notify.
	evs = w.Observe(start.Add(31*time.Minute), sys)
	if n := countNotifies(evs, "host-a.example.disk-data", "disk-used"); n != 1 {
		t.Fatalf("re-notify count = %d, want 1: %v", n, summarize(evs))
	}
	// And the clock restarts: no second re-notify 10 minutes later.
	evs = w.Observe(start.Add(41*time.Minute), sys)
	if n := countNotifies(evs, "host-a.example.disk-data", "disk-used"); n != 0 {
		t.Fatalf("re-notified again too early: %v", summarize(evs))
	}
}

// --- per-resource override precedence ----------------------------------------

func TestWatchOverridePrecedence(t *testing.T) {
	base := DefaultWatchConfig()
	// Host record says the free floor is 100G (the operator's rule for a
	// build host); the disk's own record tightens used-warn to 80.
	host := OverrideOnly(base, map[string]string{"disk-free-warn": "100G"})
	disk := OverrideOnly(host, map[string]string{"disk-warn-used": "80"})
	w := NewWatcherWithConfigs(disk, map[string]WatchConfig{
		"host-a.example.disk-data": disk,
		"host-a.example.disk-boot": host,
	})
	if disk.DiskFreeWarnBytes != 100*gib {
		t.Fatalf("host floor override lost: %d", disk.DiskFreeWarnBytes)
	}
	if disk.DiskWarnUsed != 80 {
		t.Fatalf("resource override lost: %v", disk.DiskWarnUsed)
	}
	now := time.Now().UTC()
	sys := fakeSystem()
	sys.Disks = []Disk{fakeDisk("/data", 82, 500*gib), fakeDisk("/boot", 12, 500*gib)}
	for range 3 {
		w.Observe(now, sys)
	}
	if evs := eventsFor(w, "host-a.example.disk-data", "disk-used"); len(evs) != 1 {
		t.Fatalf("disk at 82%% with warn 80 must alarm: %v", summarize(evs))
	}
	// /boot at 12% used but under the host's 100G floor? No: 500GiB disk
	// with 88% free leaves 440GiB, above the floor -> normal.
	if evs := eventsFor(w, "host-a.example.disk-boot", "disk-free"); len(evs) != 0 {
		t.Fatalf("440GiB free must not trip a 100G floor: %v", summarize(evs))
	}
}

func TestValidateWatchOverrideRejects(t *testing.T) {
	for _, kv := range [][2]string{
		{"bogus-key", "1"},
		{"disk-warn-used", "notanumber"},
		{"disk-warn-used", "101"},
		{"disk-free-warn", "12X"},
		{"cpu-sustain", "notaduration"},
		{"hysteresis", "0"},
		{"enabled", "yes-please"},
	} {
		if err := ValidateWatchOverride(kv[0], kv[1]); err == nil {
			t.Errorf("ValidateWatchOverride(%q, %q) accepted", kv[0], kv[1])
		}
	}
	for _, kv := range [][2]string{
		{"disk-warn-used", "80"},
		{"disk-free-warn", "100G"},
		{"cpu-sustain", "5m"},
		{"hysteresis", "5"},
		{"renotify", "15m"},
		{"notify-to", "operator"},
		{"link-mbps", "1000"},
		{"enabled", "false"},
	} {
		if err := ValidateWatchOverride(kv[0], kv[1]); err != nil {
			t.Errorf("ValidateWatchOverride(%q, %q) rejected: %v", kv[0], kv[1], err)
		}
	}
}

// helpers ---------------------------------------------------------------------

func eventsFor(w *Watcher, resource, metric string) []Event {
	var out []Event
	for _, e := range w.Events() {
		if e.Resource == resource && e.Metric == metric && !e.Renotify {
			out = append(out, e)
		}
	}
	return out
}

func allEvents(w *Watcher, resource, metric string) []Event {
	var out []Event
	for _, e := range w.Events() {
		if e.Resource == resource && e.Metric == metric {
			out = append(out, e)
		}
	}
	return out
}

func countNotifies(evs []Event, resource, metric string) int {
	n := 0
	for _, e := range evs {
		if e.Resource == resource && e.Metric == metric && e.Renotify {
			n++
		}
	}
	return n
}

func summarize(evs []Event) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Resource)
		b.WriteString("/")
		b.WriteString(e.Metric)
		b.WriteString("->")
		b.WriteString(e.Level.String())
		b.WriteString(" ")
	}
	return b.String()
}
