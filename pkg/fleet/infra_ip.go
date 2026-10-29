package fleet

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/provider/digitalocean"
)

func newInfraIP() *cobra.Command {
	ipCmd := &cobra.Command{
		Use:   "ip",
		Short: "Manage reserved IPs (list, attach, detach)",
	}

	ipCmd.AddCommand(newInfraIPList())
	ipCmd.AddCommand(newInfraIPAttach())
	ipCmd.AddCommand(newInfraIPDetach())

	return ipCmd
}

func newInfraIPList() *cobra.Command {
	var acct accountFlag
	var dropletID, satID int
	var region, ipAddr string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List reserved IPs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			client, err := acct.doClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx := context.Background()
			ips, err := client.ListReservedIPs(ctx)
			if err != nil {
				return err
			}

			var filtered []digitalocean.ReservedIP
			for _, r := range ips {
				if dropletID > 0 {
					if r.Droplet == nil || r.Droplet.ID != dropletID {
						continue
					}
				}
				if region != "" && !strings.EqualFold(r.Region.Slug, region) {
					continue
				}
				if ipAddr != "" && r.IP != ipAddr {
					continue
				}
				filtered = append(filtered, r)
			}
			if filtered == nil {
				filtered = []digitalocean.ReservedIP{}
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), filtered)
			}

			for _, r := range filtered {
				dID := "-"
				if r.Droplet != nil {
					dID = strconv.Itoa(r.Droplet.ID)
				}
				status := "unassigned"
				if r.Droplet != nil {
					status = "assigned"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", r.IP, r.Region.Slug, dID, status)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&dropletID, "droplet", 0, "filter by droplet ID")
	cmd.Flags().IntVar(&satID, "sat", 0, "filter by satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&region, "region", "", "filter by region")
	cmd.Flags().StringVar(&ipAddr, "ip", "", "filter by IP address")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	acct.bind(cmd)
	return cmd
}

func newInfraIPAttach() *cobra.Command {
	var acct accountFlag
	var dropletID, satID int
	var region, ipAddr string
	var dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attach a reserved IP to a droplet (create one if --ip absent; idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if dropletID <= 0 {
				return errors.New("infra ip attach: --droplet ID is required")
			}

			client, err := acct.doClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx := context.Background()

			type attachResult struct {
				DropletID int    `json:"droplet_id"`
				IP        string `json:"ip"`
				Action    string `json:"action"`
				DryRun    bool   `json:"dry_run"`
			}

			// Case 1: specific IP address given
			if ipAddr != "" {
				rip, err := client.GetReservedIP(ctx, ipAddr)
				if err != nil {
					return fmt.Errorf("infra ip attach: %w", err)
				}
				if rip.Droplet != nil && rip.Droplet.ID == dropletID {
					res := attachResult{DropletID: dropletID, IP: ipAddr, Action: "already_attached", DryRun: dryRun}
					if asJSON {
						return writeJSON(cmd.OutOrStdout(), res)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "droplet %d already has reserved IP %s\n", dropletID, ipAddr)
					return nil
				}

				if dryRun {
					res := attachResult{DropletID: dropletID, IP: ipAddr, Action: "attach", DryRun: true}
					if asJSON {
						return writeJSON(cmd.OutOrStdout(), res)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "[plan] attach reserved IP %s to droplet %d\n", ipAddr, dropletID)
					return nil
				}

				if err := client.AssignReservedIP(ctx, ipAddr, dropletID); err != nil {
					return fmt.Errorf("infra ip attach: %w", err)
				}
				res := attachResult{DropletID: dropletID, IP: ipAddr, Action: "attached", DryRun: false}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "attached reserved IP %s to droplet %d\n", ipAddr, dropletID)
				return nil
			}

			// Case 2: --ip absent: idempotent check if droplet already has ANY reserved IP
			ips, err := client.ListReservedIPs(ctx)
			if err != nil {
				return fmt.Errorf("infra ip attach: %w", err)
			}
			for _, r := range ips {
				if r.Droplet != nil && r.Droplet.ID == dropletID {
					res := attachResult{DropletID: dropletID, IP: r.IP, Action: "already_attached", DryRun: dryRun}
					if asJSON {
						return writeJSON(cmd.OutOrStdout(), res)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "droplet %d already has reserved IP %s\n", dropletID, r.IP)
					return nil
				}
			}

			// None attached: create new reserved IP
			targetRegion := region
			if targetRegion == "" {
				d, err := client.GetDroplet(ctx, dropletID)
				if err != nil {
					return fmt.Errorf("infra ip attach: get droplet %d: %w", dropletID, err)
				}
				targetRegion = d.Region.Slug
			}

			if dryRun {
				res := attachResult{DropletID: dropletID, IP: "<new>", Action: "create_and_attach", DryRun: true}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[plan] create reserved IP in region %s and attach to droplet %d\n", targetRegion, dropletID)
				return nil
			}

			newRip, err := client.CreateReservedIP(ctx, digitalocean.CreateReservedIPRequest{
				DropletID: dropletID,
				Region:    targetRegion,
			})
			if err != nil {
				return fmt.Errorf("infra ip attach: create reserved IP: %w", err)
			}

			res := attachResult{DropletID: dropletID, IP: newRip.IP, Action: "created_and_attached", DryRun: false}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created and attached reserved IP %s to droplet %d\n", newRip.IP, dropletID)
			return nil
		},
	}

	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID to attach reserved IP to")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&region, "region", "", "Region slug for newly created reserved IP")
	cmd.Flags().StringVar(&ipAddr, "ip", "", "Existing reserved IP to attach")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview action without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	acct.bind(cmd)
	return cmd
}

func newInfraIPDetach() *cobra.Command {
	var acct accountFlag
	var dropletID, satID int
	var ipAddr string
	var dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "detach",
		Short: "Detach reserved IP from a droplet (never deletes the IP; idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if dropletID <= 0 {
				return errors.New("infra ip detach: --droplet ID is required")
			}

			client, err := acct.doClient(cmd.Context())
			if err != nil {
				return err
			}
			ctx := context.Background()

			type detachResult struct {
				DropletID int    `json:"droplet_id"`
				IP        string `json:"ip"`
				Action    string `json:"action"`
				DryRun    bool   `json:"dry_run"`
			}

			ips, err := client.ListReservedIPs(ctx)
			if err != nil {
				return fmt.Errorf("infra ip detach: %w", err)
			}

			var targetIP string
			for _, r := range ips {
				if r.Droplet != nil && r.Droplet.ID == dropletID {
					if ipAddr == "" || r.IP == ipAddr {
						targetIP = r.IP
						break
					}
				}
			}

			if targetIP == "" {
				res := detachResult{DropletID: dropletID, IP: ipAddr, Action: "already_detached", DryRun: dryRun}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "droplet %d has no attached reserved IP to detach\n", dropletID)
				return nil
			}

			if dryRun {
				res := detachResult{DropletID: dropletID, IP: targetIP, Action: "detach", DryRun: true}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[plan] detach reserved IP %s from droplet %d (IP is preserved)\n", targetIP, dropletID)
				return nil
			}

			if err := client.UnassignReservedIP(ctx, targetIP); err != nil {
				return fmt.Errorf("infra ip detach: %w", err)
			}

			res := detachResult{DropletID: dropletID, IP: targetIP, Action: "detached", DryRun: false}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "detached reserved IP %s from droplet %d (IP is preserved)\n", targetIP, dropletID)
			return nil
		},
	}

	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID to detach reserved IP from")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&ipAddr, "ip", "", "Specific reserved IP to detach")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview action without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	acct.bind(cmd)
	return cmd
}
