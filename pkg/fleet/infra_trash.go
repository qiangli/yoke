package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/provider/digitalocean"
)

func newInfraTrash() *cobra.Command {
	var acct accountFlag
	var dropletID, satID int
	var grace string
	var reason string
	var dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Soft-delete a cloudbox-owned droplet (tags bashy-trash, bashy-purge-after, powers off)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if dropletID <= 0 {
				return errors.New("infra trash: --droplet ID is required")
			}

			var graceDur time.Duration
			var graceExplicit bool
			if cmd.Flags().Changed("grace") {
				var err error
				graceDur, err = time.ParseDuration(grace)
				if err != nil {
					return fmt.Errorf("infra trash: invalid --grace %q: %w", grace, err)
				}
				if graceDur <= 0 {
					return fmt.Errorf("infra trash: --grace must be positive, got %s", grace)
				}
				graceExplicit = true
			} else {
				graceDur = 24 * time.Hour
			}

			client, err := acct.doClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx := context.Background()

			res, err := client.TrashDroplet(ctx, digitalocean.TrashOptions{
				DropletID:     dropletID,
				Grace:         graceDur,
				GraceExplicit: graceExplicit,
				Reason:        reason,
				DryRun:        dryRun,
			})
			if err != nil {
				return fmt.Errorf("infra trash: %w", err)
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[plan] trash droplet %d:\n", dropletID)
				if len(res.TagsAdded) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  [tags] add %s\n", strings.Join(res.TagsAdded, ", "))
				}
				if len(res.TagsRemoved) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  [tags] remove %s\n", strings.Join(res.TagsRemoved, ", "))
				}
				if res.PoweredOff {
					fmt.Fprintf(cmd.OutOrStdout(), "  [power] power off droplet %d\n", dropletID)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  [power] droplet %d already off (noop)\n", dropletID)
				}
				return nil
			}

			if res.Status == "noop" {
				fmt.Fprintf(cmd.OutOrStdout(), "droplet %d is already trashed\n", dropletID)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "trashed droplet %d:\n", dropletID)
			if len(res.TagsAdded) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  [tags] added %s\n", strings.Join(res.TagsAdded, ", "))
			}
			if len(res.TagsRemoved) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  [tags] removed %s\n", strings.Join(res.TagsRemoved, ", "))
			}
			if res.PoweredOff {
				fmt.Fprintf(cmd.OutOrStdout(), "  [power] powered off droplet %d\n", dropletID)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID (required)")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&grace, "grace", "24h", "Grace window before purge (e.g. 24h, 1h)")
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for trashing droplet")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview trash actions without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")

	cmd.AddCommand(newInfraTrashList())
	acct.bind(cmd)
	return cmd
}

func newInfraTrashList() *cobra.Command {
	var acct accountFlag
	var asJSON bool

	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List trashed droplets with purge-after and reason",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := acct.doClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx := context.Background()

			list, err := client.ListTrashedDroplets(ctx)
			if err != nil {
				return fmt.Errorf("infra trash ls: %w", err)
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), list)
			}

			for _, d := range list {
				purgeStr := "-"
				if d.PurgeAfterRFC3339 != "" {
					purgeStr = d.PurgeAfterRFC3339
				}
				reasonStr := "-"
				if d.Reason != "" {
					reasonStr = d.Reason
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\tpurge_after:%s\treason:%s\n",
					d.DropletID, d.Name, d.Status, purgeStr, reasonStr)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	acct.bind(cmd)
	return cmd
}

func newInfraRestore() *cobra.Command {
	var acct accountFlag
	var dropletID, satID int
	var dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "restore",
		Short: "Restore a trashed droplet (removes trash tags, powers on)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if dropletID <= 0 {
				return errors.New("infra restore: --droplet ID is required")
			}

			client, err := acct.doClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx := context.Background()

			res, err := client.RestoreDroplet(ctx, digitalocean.RestoreOptions{
				DropletID: dropletID,
				DryRun:    dryRun,
			})
			if err != nil {
				return fmt.Errorf("infra restore: %w", err)
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "[plan] restore droplet %d:\n", dropletID)
				if len(res.TagsRemoved) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "  [tags] remove %s\n", strings.Join(res.TagsRemoved, ", "))
				}
				if res.PoweredOn {
					fmt.Fprintf(cmd.OutOrStdout(), "  [power] power on droplet %d\n", dropletID)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  [power] droplet %d already active (noop)\n", dropletID)
				}
				return nil
			}

			if res.Status == "noop" {
				fmt.Fprintf(cmd.OutOrStdout(), "droplet %d is already active with no trash tags\n", dropletID)
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "restored droplet %d:\n", dropletID)
			if len(res.TagsRemoved) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  [tags] removed %s\n", strings.Join(res.TagsRemoved, ", "))
			}
			if res.PoweredOn {
				fmt.Fprintf(cmd.OutOrStdout(), "  [power] powered on droplet %d\n", dropletID)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID (required)")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview restore actions without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	acct.bind(cmd)
	return cmd
}
