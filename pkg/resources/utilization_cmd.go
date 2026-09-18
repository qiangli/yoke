package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// PendingProvider reports the board's open-work reading. It is injected
// rather than imported because pkg/board already imports pkg/resources —
// the host (bashy) wires board.PendingWork in, keeping the dependency
// one-directional.
type PendingProvider func(context.Context) (PendingWork, error)

// FleetProvider reports fleet capacity. Defaults to CollectFleetResources.
type FleetProvider func(context.Context) (*FleetResources, error)

// DefaultWatchInterval is the poll gap for `resources utilization --watch`.
const DefaultWatchInterval = 30 * time.Second

// NewUtilizationCommand builds `resources utilization`: the fleet invariant
// as a checkable signal. pending may be nil, in which case the command
// reports zero pending work (and therefore OPTIMAL) — hosts are expected to
// pass their board reader.
func NewUtilizationCommand(pending PendingProvider) *cobra.Command {
	var jsonOut, watch bool
	var interval time.Duration
	fleetOf := FleetProvider(CollectFleetResources)

	cmd := &cobra.Command{
		Use:   "utilization",
		Short: "Report whether idle fleet capacity is being wasted while work is pending",
		Long: "Join fleet capacity with the board's pending work and emit exactly one verdict:\n\n" +
			"  OPTIMAL         no pending work, or all capacity busy\n" +
			"  UNDER-UTILIZED  pending work AND band-appropriate idle capacity — dispatch now\n" +
			"  SATURATED       pending work but no free capacity — honestly waiting on compute\n\n" +
			"With --watch the verdict is re-evaluated every --interval and a notification\n" +
			"is published only on the TRANSITION into UNDER-UTILIZED, never every tick.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval <= 0 {
				return fmt.Errorf("--interval must be positive")
			}
			out := cmd.OutOrStdout()
			emit := func(u *Utilization, transitioned bool) error {
				if jsonOut {
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(u)
				}
				if transitioned {
					fmt.Fprintf(out, "NOTIFY: fleet entered %s — %s\n", VerdictUnderUtilized, u.Reason)
				}
				_, err := fmt.Fprintln(out, u.Banner())
				return err
			}

			once := func(w *UtilizationWatcher) error {
				u, err := EvaluateOnce(cmd.Context(), pending, fleetOf)
				if err != nil {
					return err
				}
				return emit(u, w.Observe(u))
			}

			var w UtilizationWatcher
			if !watch {
				return once(&w)
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if err := once(&w); err != nil {
					return err
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-ticker.C:
				}
			}
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the bashy-utilization-v1 JSON envelope")
	cmd.Flags().BoolVar(&watch, "watch", false, "re-evaluate on an interval, notifying on transition into UNDER-UTILIZED")
	cmd.Flags().DurationVar(&interval, "interval", DefaultWatchInterval, "poll gap for --watch")
	return cmd
}

// EvaluateOnce collects both sides of the join and returns the verdict.
func EvaluateOnce(ctx context.Context, pending PendingProvider, fleetOf FleetProvider) (*Utilization, error) {
	var work PendingWork
	if pending != nil {
		var err error
		if work, err = pending(ctx); err != nil {
			return nil, err
		}
	}
	if fleetOf == nil {
		fleetOf = CollectFleetResources
	}
	fr, err := fleetOf(ctx)
	if err != nil {
		return nil, err
	}
	return EvaluateUtilization(time.Now().UTC(), work, fr), nil
}
