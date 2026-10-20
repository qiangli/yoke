package resources

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// NewCommand builds the singular host-resource front door: live utilization
// (system/usage/utilization) beside the fleet registry of claimable
// resources (add/rm/list/show/set/kind) from registry.go.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "resource",
		Aliases:      []string{"resources"},
		Short:        "Report host resource utilization",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(NewSystemCommand(), NewUsageCommand(), NewWatchCommand())
	// Hosts with a board reader should re-mount this with their provider:
	//   cmd.AddCommand(resources.NewUtilizationCommand(board.PendingWork))
	cmd.AddCommand(NewUtilizationCommand(nil))
	for _, sub := range newRegistryCmds() {
		cmd.AddCommand(sub)
	}
	return cmd
}

// NewSystemCommand builds `resources system`: the live system-level
// reading — CPU, memory, disk, network, GPU — as a table or as the
// bashy-resources-v1 envelope.
func NewSystemCommand() *cobra.Command {
	var jsonOut bool
	var interval time.Duration
	var minFree string
	var mountFilter string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Show live CPU, memory, disk, network, and GPU utilization for this host",
		Long: "Show live system-level resource utilization for this host.\n\n" +
			"Rates (CPU, network, disk IO) are measured by taking two counter samples\n" +
			"--interval apart and reporting the delta, so the command takes at least\n" +
			"that long to return. --interval 0 skips the second sample: CPU then\n" +
			"reports the since-boot average and byte rates are omitted.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		// The returned error is the only copy of the alarm: cobra prints it once.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < 0 {
				return fmt.Errorf("--interval must not be negative")
			}
			sys, err := Collect(cmd.Context(), Options{Interval: interval})
			if err != nil {
				return err
			}
			if minFree != "" {
				cfg, err := ParseDiskFloor(minFree)
				if err != nil {
					return fmt.Errorf("resources system: %w", err)
				}
				cfg.Mount = mountFilter
				if err := CheckDiskFloor(sys.Disks, cfg); err != nil {
					return err
				}
				if quiet {
					return nil
				}
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(sys)
			}
			return Render(cmd.OutOrStdout(), sys)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the bashy-resources-v1 JSON envelope")
	cmd.Flags().DurationVar(&interval, "interval", DefaultInterval, "gap between the two counter samples (0 = single sample, no rates)")
	cmd.Flags().StringVar(&minFree, "min-free", "", "fail if free disk space is below floor (e.g. 100G, 10%, 100G,10%)")
	cmd.Flags().StringVar(&mountFilter, "mount", "", "mount point to check (default: check candidate filesystems)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress output on success when floor is satisfied")
	return cmd
}
