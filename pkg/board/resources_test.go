package board

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/resources"
)

func resourceBoard(t *testing.T, sys *resources.System) *Board {
	t.Helper()
	src := SourceFunc{SourceName: "fixture-resources", Func: func(_ context.Context, b *Board, _ Options) error {
		b.Resources = sys
		return nil
	}}
	b, err := Collect(context.Background(), Options{Now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}, []Source{src}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func panelByID(t *testing.T, b *Board, id string) PanelView {
	t.Helper()
	for _, p := range b.Panels {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("panel %q is not in the default registry", id)
	return PanelView{}
}

func TestResourcePanelRendersEverySubsystem(t *testing.T) {
	sys := &resources.System{
		CPU:    resources.CPU{Model: "Test CPU", LogicalCores: 8, UsagePercent: 91, Pressure: "critical", Source: "ticks"},
		Memory: resources.Memory{TotalBytes: 16 << 30, UsedBytes: 8 << 30, AvailableBytes: 8 << 30, UsedPercent: 50, Pressure: "normal"},
		Disks: []resources.Disk{
			{Mount: "/", TotalBytes: 100 << 30, FreeBytes: 50 << 30, UsedPercent: 50},
			{Mount: "/data", TotalBytes: 100 << 30, FreeBytes: 1 << 30, UsedPercent: 99},
		},
		GPUs: []resources.GPU{{Name: "NVIDIA RTX 4090", VRAMBytes: 24 << 30, VRAMKind: "dedicated"}},
	}
	v := panelByID(t, resourceBoard(t, sys), "resources")
	if len(v.Rows) != 5 { // cpu + memory + 2 disks + gpu
		t.Fatalf("rows = %d: %+v", len(v.Rows), v.Rows)
	}
	if v.Rows[0][0] != "cpu" || v.Rows[0][1] != "91%" || v.Rows[0][3] != "critical" {
		t.Errorf("cpu row = %v", v.Rows[0])
	}
	// Disks sort worst-first so a nearly-full volume is the first one read.
	if v.Rows[2][0] != "disk /data" || v.Rows[2][3] != "critical" {
		t.Errorf("disk rows are not worst-first: %v", v.Rows[2:4])
	}
	if v.Rows[4][0] != "gpu" || !strings.Contains(v.Rows[4][2], "24.0GiB") {
		t.Errorf("gpu row = %v", v.Rows[4])
	}
	for _, want := range []string{"cpu 91% [critical]", "mem 50%", "disk 99% worst", "NVIDIA RTX 4090"} {
		if !strings.Contains(v.Collapsed, want) {
			t.Errorf("collapsed summary %q is missing %q", v.Collapsed, want)
		}
	}
}

func TestResourcePanelWithoutGPU(t *testing.T) {
	// A GPU-less host is the common case, not a failure: the panel says so.
	v := panelByID(t, resourceBoard(t, &resources.System{CPU: resources.CPU{LogicalCores: 2, Pressure: "normal"}}), "resources")
	last := v.Rows[len(v.Rows)-1]
	if last[0] != "gpu" || last[4] != "none detected" {
		t.Errorf("gpu row = %v", last)
	}
	if !strings.Contains(v.Collapsed, "no gpu") {
		t.Errorf("collapsed = %q", v.Collapsed)
	}
}

func TestResourcePanelWithoutReading(t *testing.T) {
	// A board built from sources that exclude the collector must still
	// render — the panel degrades, it does not panic.
	v := panelByID(t, resourceBoard(t, nil), "resources")
	if v.Collapsed != "unavailable" || len(v.Rows) != 0 {
		t.Errorf("panel = %+v", v)
	}
}

func TestResourceSourceLoadsLiveReading(t *testing.T) {
	b := &Board{}
	if err := (resourceSource{}).Load(t.Context(), b, Options{}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.Resources == nil || b.Resources.SchemaVersion != resources.SchemaVersion {
		t.Fatalf("resources = %+v", b.Resources)
	}
	if b.Resources.CPU.LogicalCores < 1 {
		t.Errorf("live reading has no cores: %+v", b.Resources.CPU)
	}
}

func TestExpandResourcesPanel(t *testing.T) {
	out, err := runCommand(t, "dashboard", "--expand", "resources")
	if err != nil {
		t.Fatalf("dashboard --expand resources: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Host resources") {
		t.Errorf("expanded output lacks the panel:\n%s", out)
	}
}

func TestDiskPressureEscalatesEarlierThanCPU(t *testing.T) {
	// A full disk stops work outright, so it crosses into elevated and
	// critical later in absolute terms but earlier in consequence than the
	// shared CPU/memory thresholds.
	for _, tt := range []struct {
		in   float64
		want string
	}{{50, "normal"}, {84.9, "normal"}, {85, "elevated"}, {94.9, "elevated"}, {95, "critical"}} {
		if got := diskPressure(tt.in); got != tt.want {
			t.Errorf("diskPressure(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
