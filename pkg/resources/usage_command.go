package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type usageCollector func(context.Context, UsageOptions) (*Usage, error)

func NewUsageCommand() *cobra.Command { return newUsageCommand(CollectUsage) }

func newUsageCommand(collect usageCollector) *cobra.Command {
	var by string
	var sprint int64
	var jsonOut, watch bool
	var interval, duration time.Duration
	cmd := &cobra.Command{
		Use:           "usage",
		Short:         "Show host totals and active weave resource usage",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sprint < 0 {
				return fmt.Errorf("--sprint must be a positive ID")
			}
			if interval < 5*time.Second {
				return fmt.Errorf("--interval must be at least 5s")
			}
			if duration < 0 {
				return fmt.Errorf("--duration cannot be negative")
			}
			if !watch && cmd.Flags().Changed("duration") {
				return fmt.Errorf("--duration requires --watch")
			}
			if _, err := GroupUsage(nil, by); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if duration > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, duration)
				defer cancel()
			}
			return runUsage(ctx, cmd.OutOrStdout(), collect, UsageOptions{Sprint: sprint}, by, jsonOut, watch, interval)
		},
	}
	cmd.Flags().StringVar(&by, "by", "repo", "group rows by repo, sprint, todo, run, or agent")
	cmd.Flags().Int64Var(&sprint, "sprint", 0, "show one sprint's active workloads")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the bashy-resource-usage-v1 JSON envelope")
	cmd.Flags().BoolVar(&watch, "watch", false, "refresh until cancelled or --duration expires")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "watch cadence (minimum 5s)")
	cmd.Flags().DurationVar(&duration, "duration", 0, "stop watching after this duration")
	return cmd
}

func runUsage(ctx context.Context, w io.Writer, collect usageCollector, opt UsageOptions, by string, jsonOut, watch bool, interval time.Duration) error {
	for {
		usage, err := collect(ctx, opt)
		if err != nil {
			return err
		}
		usage.GroupBy = by
		usage.Groups, err = GroupUsage(usage.Rows, by)
		if err != nil {
			return err
		}
		if jsonOut {
			enc := json.NewEncoder(w)
			if !watch {
				enc.SetIndent("", "  ")
			}
			if err := enc.Encode(usage); err != nil {
				return err
			}
		} else {
			if err := RenderUsage(w, usage); err != nil {
				return err
			}
		}
		if !watch {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if ctx.Err() == context.DeadlineExceeded {
				return nil
			}
			return nil
		case <-timer.C:
		}
	}
}

func RenderUsage(w io.Writer, usage *Usage) error {
	if usage == nil || usage.Host.Name == "" {
		return fmt.Errorf("host resource observation unavailable")
	}
	host := usage.Host
	if _, err := fmt.Fprintf(w, "Host %s\nCPU\t%.1f%%\t%s\nMemory\t%s / %s\t%.1f%%\nStorage\t%s / %s\n", host.Name, host.CPU.UsagePercent, host.CPU.Source, HumanBytes(host.Memory.UsedBytes), HumanBytes(host.Memory.TotalBytes), host.Memory.UsedPercent, HumanBytes(usage.Storage.UsedBytes), HumanBytes(usage.Storage.TotalBytes)); err != nil {
		return err
	}
	for _, gpu := range host.GPUs {
		utilization, used := "-", "-"
		if gpu.UtilizationPercent > 0 {
			utilization = fmt.Sprintf("%.1f%%", gpu.UtilizationPercent)
		}
		if gpu.VRAMUsedBytes > 0 {
			used = HumanBytes(gpu.VRAMUsedBytes)
		}
		if _, err := fmt.Fprintf(w, "GPU\t%s\t%s\t%s / %s\n", gpu.Name, utilization, used, HumanBytes(gpu.VRAMBytes)); err != nil {
			return err
		}
	}
	if len(host.GPUs) == 0 {
		if _, err := fmt.Fprintln(w, "GPU\tnone detected"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\nActive weave usage by %s\n", usage.GroupBy); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "GROUP\tWORKLOADS\tDISK\tCPU\tRSS"); err != nil {
		return err
	}
	for _, row := range usage.Groups {
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", row.Key, row.Workloads, humanOptionalBytes(row.WorkspaceBytes), humanOptionalPercent(row.CPUPercent), humanOptionalBytes(row.RSSBytes)); err != nil {
			return err
		}
	}
	if len(usage.Groups) == 0 {
		if _, err := fmt.Fprintln(tw, "(none)\t0\t-\t-\t-"); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, warning := range usage.Warnings {
		if _, err := fmt.Fprintln(w, "warning:", warning); err != nil {
			return err
		}
	}
	return nil
}

func humanOptionalBytes(v *uint64) string {
	if v == nil {
		return "-"
	}
	return HumanBytes(*v)
}

func humanOptionalPercent(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", *v)
}
