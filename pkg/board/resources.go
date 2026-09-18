package board

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/qiangli/yoke/pkg/resources"
)

// The resources panel answers the question the work layers cannot: is the
// HOST the bottleneck? A board full of slow runs reads very differently
// when the machine is at 98% CPU with no free disk.
//
// The panel reads the shared bounded observation cache. The board is a
// projection a steward refreshes often, so it neither sleeps for a sample nor
// starts a second workspace walk; successive cache refreshes provide rates.

type resourceSource struct{}

// NewResourceSource exposes the host-resource collector so a scoped board
// can compose it explicitly.
func NewResourceSource() Source { return resourceSource{} }

func (resourceSource) Name() string { return "resources" }

func (resourceSource) Load(ctx context.Context, b *Board, _ Options) error {
	usage, err := resources.CollectUsage(ctx, resources.UsageOptions{})
	if err != nil {
		return err
	}
	if usage.Observation != nil {
		b.Resources = usage.Observation.System
	}
	applyUsageToRuns(b.Runs, usage.Rows)
	return nil
}

func applyUsageToRuns(runs []Run, rows []resources.UsageRow) {
	byRun := make(map[string]resources.UsageRow, len(rows))
	for _, row := range rows {
		byRun[row.Repo+"#"+strconv.FormatInt(row.Run, 10)] = row
	}
	for i := range runs {
		run := &runs[i]
		row, ok := byRun[run.Repo+"#"+strconv.FormatInt(run.ID, 10)]
		if !ok {
			continue
		}
		run.SprintID, run.TodoID = row.Sprint, row.Todo
		run.CPUPercent, run.RSSBytes = row.CPUPercent.Value, row.RSSBytes.Value
		if row.WorkspaceBytes.Value != nil {
			run.WorkspaceDiskBytes = *row.WorkspaceBytes.Value
			run.WorkspaceDiskError = ""
		} else {
			run.WorkspaceDiskError = row.WorkspaceBytes.Status.Reason
		}
	}
}

func resourcePanel() Panel {
	return panel{id: "resources", build: func(b *Board) PanelView {
		v := PanelView{ID: "resources", Title: "Host resources",
			Columns: []string{"SUBSYSTEM", "USED", "CAPACITY", "PRESSURE", "DETAIL"}}
		s := b.Resources
		if s == nil {
			v.Collapsed = "unavailable"
			return v
		}
		cpuDetail := s.CPU.Source
		if s.CPU.Model != "" {
			cpuDetail = s.CPU.Model + " (" + s.CPU.Source + ")"
		}
		v.Rows = append(v.Rows, []string{"cpu", percent(s.CPU.UsagePercent), strconv.Itoa(s.CPU.LogicalCores) + " cores", s.CPU.Pressure, cpuDetail})
		v.Rows = append(v.Rows, []string{"memory", percent(s.Memory.UsedPercent), resources.HumanBytes(s.Memory.TotalBytes), s.Memory.Pressure,
			resources.HumanBytes(s.Memory.AvailableBytes) + " available"})

		worstDisk := 0.0
		disks := append([]resources.Disk(nil), s.Disks...)
		sort.Slice(disks, func(i, j int) bool { return disks[i].UsedPercent > disks[j].UsedPercent })
		for _, d := range disks {
			worstDisk = max(worstDisk, d.UsedPercent)
			v.Rows = append(v.Rows, []string{"disk " + d.Mount, percent(d.UsedPercent), resources.HumanBytes(d.TotalBytes),
				diskPressure(d.UsedPercent), resources.HumanBytes(d.FreeBytes) + " free"})
		}
		if len(s.GPUs) == 0 {
			v.Rows = append(v.Rows, []string{"gpu", "-", "-", "-", "none detected"})
		}
		for _, g := range s.GPUs {
			vram := "-"
			if g.VRAMBytes > 0 {
				vram = resources.HumanBytes(g.VRAMBytes)
				if g.VRAMKind != "" {
					vram += " " + g.VRAMKind
				}
			}
			used := "-"
			if g.UtilizationPercent > 0 {
				used = percent(g.UtilizationPercent)
			}
			v.Rows = append(v.Rows, []string{"gpu", used, vram, "-", g.Name})
		}

		gpu := "no gpu"
		if len(s.GPUs) > 0 {
			gpu = s.GPUs[0].Name
		}
		v.Collapsed = fmt.Sprintf("cpu %s [%s]; mem %s [%s]; disk %s worst; %s",
			percent(s.CPU.UsagePercent), s.CPU.Pressure, percent(s.Memory.UsedPercent), s.Memory.Pressure, percent(worstDisk), gpu)
		return v
	}}
}

// diskPressure is deliberately tighter than the CPU/memory thresholds: a
// full disk is a hard stop, not a slowdown, so it escalates earlier.
func diskPressure(p float64) string {
	switch {
	case p >= 95:
		return "critical"
	case p >= 85:
		return "elevated"
	default:
		return "normal"
	}
}

func percent(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64) + "%"
}
