package resources

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Render writes the human view: one section per subsystem, deterministic
// order, no color and no terminal probing (the agent contract — the same
// bytes whether a human or a pipe is reading).
func Render(w io.Writer, s *System) error {
	var b strings.Builder

	fmt.Fprintf(&b, "SYSTEM RESOURCES  %s (%s/%s)\n", dash(s.Host), s.OS, s.Arch)
	fmt.Fprintf(&b, "at %s  sample %ss\n\n", s.At.Format(time.RFC3339), trimFloat(s.SampleSeconds))

	fmt.Fprintf(&b, "CPU     %s%% used  [%s]  %d logical", trimFloat(s.CPU.UsagePercent), s.CPU.Pressure, s.CPU.LogicalCores)
	if s.CPU.PhysicalCores > 0 {
		fmt.Fprintf(&b, " / %d physical", s.CPU.PhysicalCores)
	}
	fmt.Fprintf(&b, " cores  (%s)\n", s.CPU.Source)
	if s.CPU.Model != "" {
		fmt.Fprintf(&b, "        model %s\n", s.CPU.Model)
	}
	if len(s.CPU.LoadAverage) == 3 {
		fmt.Fprintf(&b, "        load  %s %s %s\n", trimFloat(s.CPU.LoadAverage[0]), trimFloat(s.CPU.LoadAverage[1]), trimFloat(s.CPU.LoadAverage[2]))
	}
	if len(s.CPU.PerCorePercent) > 0 {
		fmt.Fprintf(&b, "        cores %s\n", perCoreLine(s.CPU.PerCorePercent))
	}

	m := s.Memory
	fmt.Fprintf(&b, "\nMEMORY  %s used of %s (%s%%)  [%s]  %s available\n",
		HumanBytes(m.UsedBytes), HumanBytes(m.TotalBytes), trimFloat(m.UsedPercent), m.Pressure, HumanBytes(m.AvailableBytes))
	if m.SwapTotalBytes > 0 {
		fmt.Fprintf(&b, "        swap %s used of %s\n", HumanBytes(m.SwapUsedBytes), HumanBytes(m.SwapTotalBytes))
	}

	b.WriteString("\nDISK\n")
	if len(s.Disks) == 0 {
		b.WriteString("        (none)\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  MOUNT\tFSTYPE\tSIZE\tUSED\tFREE\tUSE%\tREAD/s\tWRITE/s")
		for _, d := range s.Disks {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s%%\t%s\t%s\n", d.Mount, dash(d.FSType),
				HumanBytes(d.TotalBytes), HumanBytes(d.UsedBytes), HumanBytes(d.FreeBytes),
				trimFloat(d.UsedPercent), rateCell(d.ReadBytesPerSec), rateCell(d.WriteBytesPerSec))
		}
		tw.Flush()
	}

	b.WriteString("\nNETWORK\n")
	active := activeInterfaces(s.Network)
	if len(active) == 0 {
		b.WriteString("        (no counters)\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  IFACE\tRX\tTX\tRX/s\tTX/s")
		for _, n := range active {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", n.Name, HumanBytes(n.RxBytes), HumanBytes(n.TxBytes),
				rateCell(n.RxBytesPerSec), rateCell(n.TxBytesPerSec))
		}
		tw.Flush()
	}

	b.WriteString("\nGPU\n")
	if len(s.GPUs) == 0 {
		// Absent is the common case, not a failure.
		b.WriteString("        none detected\n")
	} else {
		for _, g := range s.GPUs {
			fmt.Fprintf(&b, "        %s", g.Name)
			if g.Vendor != "" && !strings.Contains(strings.ToLower(g.Name), strings.ToLower(g.Vendor)) {
				fmt.Fprintf(&b, " (%s)", g.Vendor)
			}
			if g.VRAMBytes > 0 {
				fmt.Fprintf(&b, "  vram %s", HumanBytes(g.VRAMBytes))
				if g.VRAMKind != "" {
					fmt.Fprintf(&b, " %s", g.VRAMKind)
				}
				if g.VRAMUsedBytes > 0 {
					fmt.Fprintf(&b, "  used %s", HumanBytes(g.VRAMUsedBytes))
				}
			}
			if g.UtilizationPercent > 0 {
				fmt.Fprintf(&b, "  util %s%%", trimFloat(g.UtilizationPercent))
			}
			b.WriteString("\n")
		}
	}

	for _, warn := range s.Warnings {
		fmt.Fprintf(&b, "\nwarning: %s", warn)
	}
	if len(s.Warnings) > 0 {
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// activeInterfaces drops the interfaces that have never carried a byte —
// a mesh host can have thirty of them and they tell the steward nothing.
func activeInterfaces(nics []Interface) []Interface {
	var out []Interface
	for _, n := range nics {
		if n.RxBytes > 0 || n.TxBytes > 0 {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].RxBytes+out[i].TxBytes > out[j].RxBytes+out[j].TxBytes
	})
	return out
}

func perCoreLine(per []float64) string {
	parts := make([]string, 0, len(per))
	for i, v := range per {
		parts = append(parts, fmt.Sprintf("%d:%s%%", i, trimFloat(v)))
	}
	return strings.Join(parts, " ")
}

func rateCell(v float64) string {
	if v <= 0 {
		return "-"
	}
	return HumanBytes(uint64(v)) + "/s"
}

// HumanBytes formats with IEC units and a fixed one-decimal shape so
// columns line up and output is byte-identical run to run.
func HumanBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return strconv.FormatUint(v, 10) + "B"
	}
	value := float64(v)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%.1f%s", value, units[i])
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
