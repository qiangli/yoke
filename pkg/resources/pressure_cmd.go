// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/artifact"
)

// NewPressureCommand builds `bashy resource pressure {evaluate,status,alerts}`:
// policy-gated resource pressure evaluation, persisted admission/throttle status,
// and bounded local emergency alert inspection.
func NewPressureCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "pressure",
		Short:        "Evaluate and inspect offline resource pressure, reclamation, and alerts",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newPressureEvaluateCommand(),
		newPressureStatusCommand(),
		newPressureAlertsCommand(),
	)
	return cmd
}

func systemToPressureObservation(ctx context.Context, sys *System, artifactRoot string) artifact.PressureObservation {
	obs := artifact.PressureObservation{
		At:                sys.At,
		Host:              sys.Host,
		CPUUsagePercent:   sys.CPU.UsagePercent,
		CPUSource:         sys.CPU.Source,
		MemTotalBytes:     sys.Memory.TotalBytes,
		MemUsedBytes:      sys.Memory.UsedBytes,
		MemAvailableBytes: sys.Memory.AvailableBytes,
		MemUsedPercent:    sys.Memory.UsedPercent,
	}

	absRoot := artifactRoot
	if abs, err := filepath.Abs(artifactRoot); err == nil {
		absRoot = abs
	}

	coveredRoot := false
	if cov, ok := coveringDisk(sys.Disks, absRoot); ok {
		for _, d := range sys.Disks {
			role := "host"
			if d.Mount == cov.Mount && !coveredRoot {
				role = "artifact"
				coveredRoot = true
			}
			obs.Disks = append(obs.Disks, artifact.DiskObservation{
				Path:              d.Mount,
				Mount:             d.Mount,
				TotalBytes:        d.TotalBytes,
				UsedBytes:         d.UsedBytes,
				FreeBytes:         d.FreeBytes,
				UsedPercent:       d.UsedPercent,
				InodesTotal:       d.InodesTotal,
				InodesUsed:        d.InodesUsed,
				InodesFree:        d.InodesFree,
				InodesUsedPercent: d.InodesUsedPercent,
				Role:              role,
			})
		}
	} else {
		for _, d := range sys.Disks {
			obs.Disks = append(obs.Disks, artifact.DiskObservation{
				Path:              d.Mount,
				Mount:             d.Mount,
				TotalBytes:        d.TotalBytes,
				UsedBytes:         d.UsedBytes,
				FreeBytes:         d.FreeBytes,
				UsedPercent:       d.UsedPercent,
				InodesTotal:       d.InodesTotal,
				InodesUsed:        d.InodesUsed,
				InodesFree:        d.InodesFree,
				InodesUsedPercent: d.InodesUsedPercent,
				Role:              "host",
			})
		}
	}

	if !coveredRoot && strings.TrimSpace(absRoot) != "" {
		if dObs, err := artifact.StatFilesystemPath(ctx, absRoot, "artifact"); err == nil {
			obs.Disks = append(obs.Disks, dObs)
		}
	}
	return obs
}

func newPressureEvaluateCommand() *cobra.Command {
	var (
		artifactRoot string
		profileSpec  string
		ollamaURL    string
		activate     bool
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:           "evaluate",
		Short:         "Run one host/VM pressure observation and policy-gated reclamation pass",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			root := strings.TrimSpace(artifactRoot)
			if root == "" {
				root = artifact.DefaultRoot()
			}

			opts := artifact.PressureControllerOptions{
				Root:          root,
				ProfileSpec:   profileSpec,
				OllamaBaseURL: ollamaURL,
				ObserveFunc: func(octx context.Context) (artifact.PressureObservation, error) {
					sys, err := WatchSampler(octx)
					if err != nil {
						return artifact.PressureObservation{}, err
					}
					return systemToPressureObservation(octx, sys, root), nil
				},
			}

			if activate {
				var prof artifact.ProfileV1
				if strings.TrimSpace(profileSpec) != "" {
					p, err := artifact.ResolveProfile(profileSpec, artifact.CurrentPlatform())
					if err != nil {
						return err
					}
					prof = p
				} else if saved, ok, err := artifact.LoadSavedProfile(root); err == nil && ok {
					prof = saved
				} else {
					prof = artifact.DefaultProfileV1(artifact.CurrentPlatform())
				}
				prof.Resources.Activated = true
				opts.Profile = &prof
				opts.ProfileSpec = ""
			}

			res, err := artifact.NewPressureController(opts).Evaluate(ctx)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			renderPressureResult(cmd.OutOrStdout(), res)
			return nil
		},
	}
	cmd.Flags().StringVar(&artifactRoot, "artifact-root", "", "artifact store root directory (default: managed bashy artifacts dir)")
	cmd.Flags().StringVar(&profileSpec, "profile", "", "offline profile name or JSON path")
	cmd.Flags().StringVar(&ollamaURL, "ollama-url", "", "managed Ollama HTTP base URL (default: http://127.0.0.1:11435)")
	cmd.Flags().BoolVar(&activate, "activate", false, "enable policy-gated reclamation, model memory unload, and growth admission blocking")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit structured JSON verdict")
	return cmd
}

func newPressureStatusCommand() *cobra.Command {
	var (
		artifactRoot string
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Show persisted resource pressure state and recovery hysteresis progress",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := strings.TrimSpace(artifactRoot)
			if root == "" {
				root = artifact.DefaultRoot()
			}
			st, ok, err := artifact.LoadPressureState(root)
			if err != nil {
				return err
			}
			if !ok {
				activated := false
				if saved, sOK, sErr := artifact.LoadSavedProfile(root); sErr == nil && sOK {
					activated = saved.Resources.Activated
				}
				st = artifact.PressureStateV1{
					SchemaVersion: artifact.PressureStateSchemaV1,
					Activated:     activated,
					Level:         artifact.PressureNormal,
					ObservedLevel: artifact.PressureNormal,
				}
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			renderPressureState(cmd.OutOrStdout(), root, st)
			return nil
		},
	}
	cmd.Flags().StringVar(&artifactRoot, "artifact-root", "", "artifact store root directory")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit structured JSON status")
	return cmd
}

func newPressureAlertsCommand() *cobra.Command {
	var (
		artifactRoot string
		limit        int
		jsonOut      bool
	)
	cmd := &cobra.Command{
		Use:           "alerts",
		Short:         "Read bounded local emergency pressure alerts from alerts.jsonl",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := strings.TrimSpace(artifactRoot)
			if root == "" {
				root = artifact.DefaultRoot()
			}
			records, err := artifact.LoadAlertRecords(root, limit)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(records)
			}
			if len(records) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no pressure alerts recorded")
				return nil
			}
			for _, r := range records {
				fmt.Fprintf(cmd.OutOrStdout(), "%s  event=%s  to=%s  blocked=%v  unsatisfied_floor=%v",
					r.At.Format("2006-01-02T15:04:05Z07:00"),
					r.EventType, r.ToLevel, r.GrowthBlocked, r.UnsatisfiedFloor)
				if r.ReclaimedBytes > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  reclaimed=%s(%d)", HumanBytes(uint64(r.ReclaimedBytes)), r.ReclaimedItems)
				}
				if len(r.UnloadedModels) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  unloaded=%s", strings.Join(r.UnloadedModels, ","))
				}
				if len(r.Reasons) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  reasons=%s", strings.Join(r.Reasons, "; "))
				}
				fmt.Fprintln(cmd.OutOrStdout())
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&artifactRoot, "artifact-root", "", "artifact store root directory")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum newest alert records to return (0 = all)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON array of alert records")
	return cmd
}

func renderPressureState(w io.Writer, root string, st artifact.PressureStateV1) {
	mode := "advisory"
	if st.Activated {
		mode = "activated"
	}
	fmt.Fprintf(w, "artifact_root:        %s\n", root)
	fmt.Fprintf(w, "mode:                 %s\n", mode)
	fmt.Fprintf(w, "level:                %s (observed: %s)\n", st.Level, st.ObservedLevel)
	fmt.Fprintf(w, "growth_blocked:       %v\n", st.GrowthBlocked)
	fmt.Fprintf(w, "throttle_concurrency: %d\n", st.ThrottleConcurrency)
	fmt.Fprintf(w, "unsatisfied_floor:    %v\n", st.UnsatisfiedFloor)
	fmt.Fprintf(w, "healthy_samples:      %d\n", st.HealthySamples)
	if len(st.Reasons) > 0 {
		fmt.Fprintf(w, "reasons:              %s\n", strings.Join(st.Reasons, "; "))
	}
}

func renderPressureResult(w io.Writer, res artifact.PressureResult) {
	renderPressureState(w, res.ArtifactRoot, res.State)
	fmt.Fprintf(w, "reclaimed:            %d item(s), %s\n", res.ReclaimedItems, HumanBytes(uint64(max(res.ReclaimedBytes, 0))))
	if len(res.UnloadedModels) > 0 {
		fmt.Fprintf(w, "unloaded_models:      %s\n", strings.Join(res.UnloadedModels, ", "))
	}
}
