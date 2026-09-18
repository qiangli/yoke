package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCollectLiveHost is the contract check: on every platform this repo
// ships, a reading has a real timestamp, real core count, real memory, and
// at least one filesystem. It runs against the host, so it asserts shape
// and plausibility rather than exact numbers.
func TestCollectLiveHost(t *testing.T) {
	sys, err := Collect(t.Context(), Options{Interval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if sys.SchemaVersion != SchemaVersion {
		t.Errorf("schema = %q", sys.SchemaVersion)
	}
	if sys.At.IsZero() {
		t.Error("At is zero — every reading must be timestamped")
	}
	if sys.OS != runtime.GOOS || sys.Arch != runtime.GOARCH {
		t.Errorf("os/arch = %s/%s", sys.OS, sys.Arch)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skipf("no probes implemented for %s; warnings: %v", runtime.GOOS, sys.Warnings)
	}
	if sys.CPU.LogicalCores < 1 {
		t.Error("logical cores must be at least 1")
	}
	if sys.CPU.Source == "unavailable" || sys.CPU.Pressure == "" {
		t.Errorf("cpu unavailable: %+v (warnings %v)", sys.CPU, sys.Warnings)
	}
	if sys.Memory.TotalBytes == 0 || sys.Memory.UsedBytes == 0 {
		t.Errorf("memory = %+v (warnings %v)", sys.Memory, sys.Warnings)
	}
	if sys.Memory.UsedPercent <= 0 || sys.Memory.UsedPercent > 100 {
		t.Errorf("memory used%% = %v", sys.Memory.UsedPercent)
	}
	if len(sys.Disks) == 0 {
		t.Errorf("no filesystems reported (warnings %v)", sys.Warnings)
	}
	for _, d := range sys.Disks {
		if d.TotalBytes == 0 || d.Mount == "" {
			t.Errorf("degenerate disk entry %+v", d)
		}
	}
	// GPU absence is normal; a reported GPU must at least be named.
	for _, g := range sys.GPUs {
		if g.Name == "" {
			t.Errorf("unnamed GPU entry %+v", g)
		}
	}
	if sys.SampleSeconds <= 0 {
		t.Errorf("sample window = %v", sys.SampleSeconds)
	}
}

// TestCollectSingleSample pins the cheap path the board panel uses: no
// sleep, no invented rates, still a usable CPU figure.
func TestCollectSingleSample(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no probes on " + runtime.GOOS)
	}
	start := time.Now()
	sys, err := Collect(t.Context(), Options{Interval: 0})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("single-sample collect took %v — it must not sleep", elapsed)
	}
	if sys.SampleSeconds != 0 {
		t.Errorf("sample window = %v, want 0", sys.SampleSeconds)
	}
	for _, n := range sys.Network {
		if n.RxBytesPerSec != 0 || n.TxBytesPerSec != 0 {
			t.Errorf("interface %s reported a rate without a second sample", n.Name)
		}
	}
	if sys.CPU.Source == "ticks" {
		t.Error("cpu claims a sampled delta from a single sample")
	}
}

func TestCollectJSONEnvelope(t *testing.T) {
	sys, err := Collect(t.Context(), Options{Interval: 0, Now: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	b, err := json.Marshal(sys)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"schema_version", "at", "os", "arch", "cpu", "memory"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("envelope is missing %q", key)
		}
	}
	if decoded["schema_version"] != SchemaVersion {
		t.Errorf("schema_version = %v", decoded["schema_version"])
	}
	if !strings.HasPrefix(decoded["at"].(string), "2023-11-14T") {
		t.Errorf("at = %v — Options.Now must win", decoded["at"])
	}
}

func TestTickUsage(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur cpuTicks
		want      float64
	}{
		{"half busy", cpuTicks{100, 50}, cpuTicks{200, 100}, 50},
		{"fully idle", cpuTicks{0, 0}, cpuTicks{100, 100}, 0},
		{"fully busy", cpuTicks{0, 0}, cpuTicks{100, 0}, 100},
		{"no movement", cpuTicks{100, 50}, cpuTicks{100, 50}, 0},
		// A counter that went backwards means a reset (or a container
		// boundary): report 0 rather than a garbage spike.
		{"counter reset", cpuTicks{500, 100}, cpuTicks{100, 20}, 0},
		// Idle exceeding total cannot happen; clamp instead of underflowing.
		{"idle overshoot", cpuTicks{0, 0}, cpuTicks{100, 150}, 0},
	}
	for _, tt := range tests {
		if got := tickUsage(tt.prev, tt.cur); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestBuildNetworkRates(t *testing.T) {
	first := []netCounter{{name: "eth0", rxBytes: 1000, txBytes: 500}, {name: "lo", rxBytes: 10, txBytes: 10}}
	second := []netCounter{{name: "eth0", rxBytes: 3000, txBytes: 1500}, {name: "wg0", rxBytes: 77, txBytes: 0}}
	got := buildNetwork(first, second, 2)
	if len(got) != 2 || got[0].Name != "eth0" || got[1].Name != "wg0" {
		t.Fatalf("got %+v — interfaces must be name-sorted and follow the second sample", got)
	}
	if got[0].RxBytesPerSec != 1000 || got[0].TxBytesPerSec != 500 {
		t.Errorf("eth0 rates = %+v", got[0])
	}
	// An interface that appeared between samples has no baseline: report
	// the counter, not a rate computed from zero.
	if got[1].RxBytesPerSec != 0 {
		t.Errorf("wg0 invented a rate: %+v", got[1])
	}
}

func TestApplyDiskIO(t *testing.T) {
	disks := []Disk{{Mount: "/", Device: "/dev/sda1"}, {Mount: "/data", Device: "/dev/sdb1"}}
	first := []ioCounter{{device: "sda1", readBytes: 1000, writeBytes: 2000}}
	second := []ioCounter{{device: "sda1", readBytes: 5000, writeBytes: 2000}}
	got := applyDiskIO(disks, first, second, 4)
	if got[0].ReadBytesPerSec != 1000 || got[0].WriteBytesPerSec != 0 {
		t.Errorf("/ rates = %+v", got[0])
	}
	if got[1].ReadBytesPerSec != 0 {
		t.Errorf("/data has no counters; it must stay bare: %+v", got[1])
	}
}

func TestDedupeByKey(t *testing.T) {
	disks := []Disk{
		{Mount: "/System/Volumes/Data", Device: "/dev/disk3s5", TotalBytes: 100},
		{Mount: "/", Device: "/dev/disk3s1s1", TotalBytes: 100},
		{Mount: "/data", Device: "/dev/disk4s1", TotalBytes: 200},
	}
	got := dedupeByKey(disks, func(d Disk) string { return d.Device[:len("/dev/disk")+1] })
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Mount != "/" {
		t.Errorf("shortest mount must win, got %q", got[0].Mount)
	}
	// The portable key is the device, which leaves distinct devices alone.
	if len(dedupeByKey(disks, byDevice)) != 3 {
		t.Error("distinct devices were collapsed")
	}
}

func TestClassify(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want string
	}{{0, "normal"}, {74.9, "normal"}, {75, "elevated"}, {89.9, "elevated"}, {90, "critical"}, {100, "critical"}} {
		if got := classify(tt.in); got != tt.want {
			t.Errorf("classify(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[uint64]string{
		0:           "0B",
		512:         "512B",
		1024:        "1.0KiB",
		1536:        "1.5KiB",
		1024 * 1024: "1.0MiB",
		25769803776: "24.0GiB",
		1 << 50:     "1.0PiB",
	} {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderHumanView(t *testing.T) {
	sys := &System{
		SchemaVersion: SchemaVersion,
		At:            time.Unix(1700000000, 0).UTC(),
		Host:          "testhost",
		OS:            "linux",
		Arch:          "amd64",
		SampleSeconds: 0.3,
		CPU:           CPU{Model: "Test CPU", LogicalCores: 4, UsagePercent: 42.5, PerCorePercent: []float64{40, 45}, LoadAverage: []float64{1, 2, 3}, Pressure: "normal", Source: "ticks"},
		Memory:        Memory{TotalBytes: 8 << 30, UsedBytes: 4 << 30, AvailableBytes: 4 << 30, UsedPercent: 50, Pressure: "normal"},
		Disks:         []Disk{{Mount: "/", FSType: "ext4", TotalBytes: 100 << 30, UsedBytes: 60 << 30, FreeBytes: 40 << 30, UsedPercent: 60, ReadBytesPerSec: 2048}},
		Network:       []Interface{{Name: "eth0", RxBytes: 1 << 20, TxBytes: 1 << 20, RxBytesPerSec: 1024}, {Name: "dead0"}},
		Warnings:      []string{"gpu: probe failed"},
	}
	var buf bytes.Buffer
	if err := Render(&buf, sys); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"testhost (linux/amd64)",
		"2023-11-14T",
		"CPU     42.5% used  [normal]  4 logical",
		"model Test CPU",
		"load  1 2 3",
		"cores 0:40% 1:45%",
		"MEMORY  4.0GiB used of 8.0GiB (50%)",
		"/", "ext4", "2.0KiB/s",
		"eth0",
		"none detected", // no GPUs
		"warning: gpu: probe failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render is missing %q:\n%s", want, out)
		}
	}
	// An interface that has never carried a byte is noise, not signal.
	if strings.Contains(out, "dead0") {
		t.Errorf("silent interface was listed:\n%s", out)
	}
}

func TestRenderGPUPresent(t *testing.T) {
	sys := &System{GPUs: []GPU{{Vendor: "Apple", Name: "Apple M4 Pro GPU", VRAMBytes: 24 << 30, VRAMKind: "unified"}}}
	var buf bytes.Buffer
	if err := Render(&buf, sys); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Apple M4 Pro GPU") || !strings.Contains(out, "vram 24.0GiB unified") {
		t.Errorf("GPU line missing VRAM:\n%s", out)
	}
	if strings.Contains(out, "none detected") {
		t.Errorf("GPU present but rendered as none:\n%s", out)
	}
}

func TestCollectHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Collect(ctx, Options{Interval: 5 * time.Second}); err == nil {
		t.Fatal("cancelled context must abort the sample wait")
	}
}
