package resources

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/qiangli/yoke/pkg/weave"
)

const UsageSchemaVersion = "bashy-resource-usage-v1"

type UsageOptions struct {
	CacheDir string
	Sprint   int64
}

type StorageTotals struct {
	UsedBytes  uint64 `json:"used_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

type Usage struct {
	SchemaVersion     string           `json:"schema_version"`
	At                time.Time        `json:"at"`
	Host              UsageHost        `json:"host"`
	Storage           StorageTotals    `json:"storage"`
	Rows              []UsageRow       `json:"rows"`
	GroupBy           string           `json:"group_by"`
	Groups            []UsageGroup     `json:"groups"`
	InventoryComplete bool             `json:"inventory_complete"`
	Observation       *HostObservation `json:"-"`
	Warnings          []string         `json:"warnings,omitempty"`
}

type UsageHost struct {
	At       time.Time                    `json:"at"`
	Name     string                       `json:"name,omitempty"`
	OS       string                       `json:"os"`
	Arch     string                       `json:"arch"`
	CPU      CPU                          `json:"cpu"`
	Memory   Memory                       `json:"memory"`
	Disks    []Disk                       `json:"disks"`
	GPUs     []GPU                        `json:"gpus"`
	Sections map[string]ObservationStatus `json:"sections"`
}

type UsageRow struct {
	ID             string                    `json:"id"`
	Repo           string                    `json:"repo"`
	Sprint         int64                     `json:"sprint,omitempty"`
	Todo           string                    `json:"todo,omitempty"`
	Run            int64                     `json:"run"`
	Agent          string                    `json:"agent,omitempty"`
	State          string                    `json:"state"`
	Workspace      string                    `json:"workspace,omitempty"`
	WorkspaceBytes ObservationValue[uint64]  `json:"workspace_bytes"`
	CPUPercent     ObservationValue[float64] `json:"cpu_percent"`
	RSSBytes       ObservationValue[uint64]  `json:"rss_bytes"`
}

type UsageGroup struct {
	Key            string   `json:"key"`
	Workloads      int      `json:"workloads"`
	WorkspaceBytes *uint64  `json:"workspace_bytes"`
	CPUPercent     *float64 `json:"cpu_percent"`
	RSSBytes       *uint64  `json:"rss_bytes"`
}

func CollectUsage(ctx context.Context, opt UsageOptions) (*Usage, error) {
	cacheDir := opt.CacheDir
	if cacheDir == "" {
		cacheDir = ResourcesStateDir()
	}
	out := &Usage{SchemaVersion: UsageSchemaVersion, At: time.Now().UTC()}
	inv, err := weave.ReadSprintInventory(ctx, cacheDir)
	if err != nil {
		out.Warnings = append(out.Warnings, "workload inventory: "+err.Error())
	} else {
		out.InventoryComplete = inv.Complete
		out.Warnings = append(out.Warnings, inv.Warnings...)
	}
	if opt.Sprint > 0 && inv != nil && inv.Complete {
		found := false
		for _, seat := range inv.Sprints {
			found = found || seat.ID == opt.Sprint
		}
		if !found {
			return nil, fmt.Errorf("sprint #%d not found", opt.Sprint)
		}
	}

	var refs []WorkloadRef
	var roots []ScanRoot
	seenRoots := map[string]bool{}
	if inv != nil {
		for _, row := range inv.Workloads {
			refs = append(refs, WorkloadRef{ID: row.ID, Agent: row.Agent, Sprint: strconv.FormatInt(row.Sprint, 10), Run: strconv.FormatInt(row.Run, 10), Workspace: row.Workspace, Root: ProcessIdentity{PID: row.PID, StartID: row.StartID}, RegisteredAt: row.StartedAt})
			if row.Workspace != "" && !seenRoots[row.Workspace] {
				if len(roots) == maxObservationScanRoots {
					out.Warnings = append(out.Warnings, "workspace scan root limit reached")
					continue
				}
				seenRoots[row.Workspace] = true
				roots = append(roots, ScanRoot{Path: row.Workspace, WorkloadID: row.ID, Kind: "workspace"})
			}
		}
	}
	host, err := ObserveHost(ctx, HostObserveOptions{CacheDir: cacheDir, Workloads: refs, ScanRoots: roots})
	if err != nil {
		return nil, err
	}
	out.Observation = host
	out.At = host.At
	// Surface stale orphan test processes (a leaked, timed-out gate's residue)
	// so their CPU has an owner in the diagnostics rather than distorting alerts
	// silently. See pkg/resources/orphans.go and the pkg/gate process-group fix.
	out.Warnings = append(out.Warnings, orphanTestProcessWarnings(host.Processes)...)
	out.Host = usageHost(host)
	out.Storage = storageTotals(host.System)
	workloads := map[string]WorkloadObservation{}
	for _, row := range host.Workloads {
		workloads[row.ID] = row
	}
	directories := map[string]DirectoryObservation{}
	for _, row := range host.Directories {
		directories[row.WorkloadID] = row
	}
	if inv != nil {
		for _, row := range inv.Workloads {
			if opt.Sprint > 0 && row.Sprint != opt.Sprint {
				continue
			}
			usage := UsageRow{ID: row.ID, Repo: row.Repo, Sprint: row.Sprint, Todo: row.Todo, Run: row.Run, Agent: row.Agent, State: row.State, Workspace: row.Workspace}
			usage.CPUPercent = unknownValue[float64]("process-tree", "workload process unavailable", out.At)
			usage.RSSBytes = unknownValue[uint64]("process-tree", "workload process unavailable", out.At)
			usage.WorkspaceBytes = unknownValue[uint64]("directory", "workspace scan unavailable", out.At)
			if observed, ok := workloads[row.ID]; ok {
				usage.CPUPercent, usage.RSSBytes = observed.CPU, observed.RSS
			}
			if observed, ok := directories[row.ID]; ok {
				usage.WorkspaceBytes = observed.Bytes
			}
			out.Rows = append(out.Rows, usage)
		}
	}
	return out, nil
}

func usageHost(observation *HostObservation) UsageHost {
	out := UsageHost{Sections: map[string]ObservationStatus{}}
	if observation == nil || observation.System == nil {
		return out
	}
	sys := observation.System
	out.At, out.Name, out.OS, out.Arch = observation.At, sys.Host, sys.OS, sys.Arch
	out.CPU, out.Memory, out.Disks, out.GPUs = sys.CPU, sys.Memory, sys.Disks, sys.GPUs
	for _, key := range []string{"cpu", "memory", "disks", "gpu", "processes"} {
		if status, ok := observation.Sections[key]; ok {
			out.Sections[key] = status
		}
	}
	return out
}

func storageTotals(sys *System) StorageTotals {
	var out StorageTotals
	if sys == nil {
		return out
	}
	seen := map[string]bool{}
	for _, disk := range sys.Disks {
		key := disk.Device
		if key == "" {
			key = disk.Mount
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out.UsedBytes += disk.UsedBytes
		out.TotalBytes += disk.TotalBytes
	}
	return out
}

func GroupUsage(rows []UsageRow, by string) ([]UsageGroup, error) {
	if by == "" {
		by = "repo"
	}
	valid := map[string]bool{"repo": true, "sprint": true, "todo": true, "run": true, "agent": true}
	if !valid[by] {
		return nil, fmt.Errorf("unknown grouping %q (want repo, sprint, todo, run, or agent)", by)
	}
	type sums struct {
		UsageGroup
		diskKnown, cpuKnown, rssKnown bool
		disk                          uint64
		cpu                           float64
		rss                           uint64
	}
	groups := map[string]*sums{}
	for _, row := range rows {
		key := usageGroupKey(row, by)
		g := groups[key]
		if g == nil {
			g = &sums{UsageGroup: UsageGroup{Key: key}}
			groups[key] = g
		}
		g.Workloads++
		if row.WorkspaceBytes.Value != nil {
			g.disk += *row.WorkspaceBytes.Value
			g.diskKnown = true
		}
		if row.CPUPercent.Value != nil {
			g.cpu += *row.CPUPercent.Value
			g.cpuKnown = true
		}
		if row.RSSBytes.Value != nil {
			g.rss += *row.RSSBytes.Value
			g.rssKnown = true
		}
	}
	out := make([]UsageGroup, 0, len(groups))
	for _, g := range groups {
		if g.diskKnown {
			v := g.disk
			g.WorkspaceBytes = &v
		}
		if g.cpuKnown {
			v := g.cpu
			g.CPUPercent = &v
		}
		if g.rssKnown {
			v := g.rss
			g.RSSBytes = &v
		}
		out = append(out, g.UsageGroup)
	}
	sort.Slice(out, func(i, j int) bool {
		var a, b uint64
		if out[i].WorkspaceBytes != nil {
			a = *out[i].WorkspaceBytes
		}
		if out[j].WorkspaceBytes != nil {
			b = *out[j].WorkspaceBytes
		}
		if a != b {
			return a > b
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func usageGroupKey(row UsageRow, by string) string {
	var key string
	switch by {
	case "repo":
		key = row.Repo
	case "sprint":
		if row.Sprint > 0 {
			key = strconv.FormatInt(row.Sprint, 10)
		}
	case "todo":
		key = row.Todo
	case "run":
		key = strconv.FormatInt(row.Run, 10)
	case "agent":
		key = row.Agent
	}
	if key == "" {
		return "(none)"
	}
	return key
}
