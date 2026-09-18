package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGroupUsageDimensionsAndOrdering(t *testing.T) {
	diskA, diskB := uint64(100), uint64(300)
	cpuA, cpuB := 1.5, 2.5
	rssA, rssB := uint64(10), uint64(20)
	rows := []UsageRow{
		{Repo: "/repo/a", Sprint: 177, Todo: "todo-a", Run: 1, Agent: "agent-a", WorkspaceBytes: ObservationValue[uint64]{Value: &diskA}, CPUPercent: ObservationValue[float64]{Value: &cpuA}, RSSBytes: ObservationValue[uint64]{Value: &rssA}},
		{Repo: "/repo/b", Sprint: 177, Run: 2, Agent: "agent-b", WorkspaceBytes: ObservationValue[uint64]{Value: &diskB}, CPUPercent: ObservationValue[float64]{Value: &cpuB}, RSSBytes: ObservationValue[uint64]{Value: &rssB}},
	}
	for _, by := range []string{"repo", "sprint", "todo", "run", "agent"} {
		groups, err := GroupUsage(rows, by)
		if err != nil {
			t.Fatalf("group %s: %v", by, err)
		}
		if len(groups) == 0 {
			t.Fatalf("group %s returned no rows", by)
		}
	}
	groups, _ := GroupUsage(rows, "repo")
	if groups[0].Key != "/repo/b" || *groups[0].WorkspaceBytes != 300 {
		t.Fatalf("groups not disk-descending: %+v", groups)
	}
	groups, _ = GroupUsage(rows, "sprint")
	if len(groups) != 1 || groups[0].Workloads != 2 || *groups[0].CPUPercent != 4 || *groups[0].RSSBytes != 30 {
		t.Fatalf("sprint aggregation = %+v", groups)
	}
	if _, err := GroupUsage(rows, "host"); err == nil {
		t.Fatal("unknown grouping accepted")
	}
}

func TestStorageTotalsDeduplicatesDevices(t *testing.T) {
	got := storageTotals(&System{Disks: []Disk{
		{Device: "disk0", Mount: "/", UsedBytes: 40, TotalBytes: 100},
		{Device: "disk0", Mount: "/duplicate", UsedBytes: 40, TotalBytes: 100},
		{Device: "disk1", Mount: "/data", UsedBytes: 20, TotalBytes: 200},
	}})
	if got.UsedBytes != 60 || got.TotalBytes != 300 {
		t.Fatalf("storage totals = %+v", got)
	}
}

func TestUsageCommandJSONAndHuman(t *testing.T) {
	now := time.Now().UTC()
	disk, cpu, rss := uint64(1024), 12.5, uint64(2048)
	collect := func(context.Context, UsageOptions) (*Usage, error) {
		return &Usage{SchemaVersion: UsageSchemaVersion, At: now, Host: UsageHost{Name: "fixture", CPU: CPU{UsagePercent: 25, Source: "ticks"}, Memory: Memory{UsedBytes: 50, TotalBytes: 100, UsedPercent: 50}}, Storage: StorageTotals{UsedBytes: 10, TotalBytes: 20}, Rows: []UsageRow{{Repo: "/repo", Sprint: 177, WorkspaceBytes: ObservationValue[uint64]{Value: &disk}, CPUPercent: ObservationValue[float64]{Value: &cpu}, RSSBytes: ObservationValue[uint64]{Value: &rss}}}}, nil
	}
	cmd := newUsageCommand(collect)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json", "--by", "sprint"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got Usage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != UsageSchemaVersion || got.GroupBy != "sprint" || len(got.Groups) != 1 {
		t.Fatalf("usage JSON = %+v", got)
	}

	out.Reset()
	cmd = newUsageCommand(collect)
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Host fixture", "Storage", "Active weave usage by repo", "/repo", "1.0KiB"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human output missing %q:\n%s", want, out.String())
		}
	}
}
