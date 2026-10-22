package fleet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/provider/digitalocean"
)

type HardenAction struct {
	Dimension   string `json:"dimension"`
	Action      string `json:"action"`
	Target      string `json:"target,omitempty"`
	Status      string `json:"status"` // "noop", "planned", "reconciled"
	Description string `json:"description"`
}

type HardenResult struct {
	DropletID  int            `json:"droplet_id"`
	DryRun     bool           `json:"dry_run"`
	Reconciled bool           `json:"reconciled"`
	Actions    []HardenAction `json:"actions"`
}

func newInfraHarden() *cobra.Command {
	var dropletID, satID int
	var projectName, firewallName string
	var tags []string
	var reservedIP, dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "harden",
		Short: "Reconcile droplet hardening (project, tags, firewall membership, reserved IP)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if dropletID <= 0 {
				return errors.New("infra harden: --droplet ID is required")
			}

			client, err := resolveDOClient()
			if err != nil {
				return err
			}
			ctx := context.Background()

			// 1. Fetch current droplet state
			droplet, err := client.GetDroplet(ctx, dropletID)
			if err != nil {
				return fmt.Errorf("infra harden: droplet %d: %w", dropletID, err)
			}

			var actions []HardenAction

			// 2. Project assignment reconciliation
			if projectName != "" {
				p, err := client.FindProjectByNameOrID(ctx, projectName)
				if err != nil {
					return fmt.Errorf("infra harden: %w", err)
				}

				resources, err := client.ListProjectResources(ctx, p.ID)
				if err != nil {
					return fmt.Errorf("infra harden: list project %s resources: %w", p.Name, err)
				}
				urn := fmt.Sprintf("do:droplet:%d", dropletID)
				alreadyInProject := false
				for _, r := range resources {
					if r.URN == urn {
						alreadyInProject = true
						break
					}
				}

				if alreadyInProject {
					actions = append(actions, HardenAction{
						Dimension:   "project",
						Action:      "assign",
						Target:      p.Name,
						Status:      "noop",
						Description: fmt.Sprintf("droplet %d is already assigned to project %q", dropletID, p.Name),
					})
				} else {
					status := "planned"
					if !dryRun {
						if err := client.AssignProjectResources(ctx, p.ID, []string{urn}); err != nil {
							return fmt.Errorf("infra harden: assign droplet %d to project %s: %w", dropletID, p.Name, err)
						}
						status = "reconciled"
					}
					actions = append(actions, HardenAction{
						Dimension:   "project",
						Action:      "assign",
						Target:      p.Name,
						Status:      status,
						Description: fmt.Sprintf("assign droplet %d to project %q", dropletID, p.Name),
					})
				}
			}

			// 3. Tags reconciliation
			var missingTags []string
			currentTags := slices.Clone(droplet.Tags)
			for _, t := range tags {
				t = strings.TrimSpace(t)
				if t == "" {
					continue
				}
				if !slices.Contains(currentTags, t) {
					missingTags = append(missingTags, t)
				}
			}

			if len(missingTags) == 0 && len(tags) > 0 {
				actions = append(actions, HardenAction{
					Dimension:   "tags",
					Action:      "add",
					Target:      strings.Join(tags, ","),
					Status:      "noop",
					Description: fmt.Sprintf("droplet %d already has all specified tags", dropletID),
				})
			} else if len(missingTags) > 0 {
				status := "planned"
				if !dryRun {
					for _, mt := range missingTags {
						if err := client.TagDroplet(ctx, mt, dropletID); err != nil {
							return fmt.Errorf("infra harden: tag droplet %d with %q: %w", dropletID, mt, err)
						}
						currentTags = append(currentTags, mt)
					}
					status = "reconciled"
				}
				actions = append(actions, HardenAction{
					Dimension:   "tags",
					Action:      "add",
					Target:      strings.Join(missingTags, ","),
					Status:      status,
					Description: fmt.Sprintf("add tags %s to droplet %d", strings.Join(missingTags, ","), dropletID),
				})
			}

			// 4. Firewall membership via tag reconciliation
			if firewallName != "" {
				fw, err := client.FindFirewallByName(ctx, firewallName)
				if err != nil {
					return fmt.Errorf("infra harden: lookup firewall %q: %w", firewallName, err)
				}

				if fw == nil {
					// Firewall does not exist: create it with linking tag
					linkTag := firewallName
					if len(tags) > 0 && tags[0] != "" {
						linkTag = tags[0]
					}
					status := "planned"
					if !dryRun {
						if !slices.Contains(currentTags, linkTag) {
							if err := client.TagDroplet(ctx, linkTag, dropletID); err != nil {
								return fmt.Errorf("infra harden: tag droplet %d: %w", dropletID, err)
							}
							currentTags = append(currentTags, linkTag)
						}
						newFW := &digitalocean.Firewall{
							Name:          firewallName,
							Tags:          []string{linkTag},
							OutboundRules: digitalocean.DefaultOutboundRules(),
						}
						if _, err := client.CreateFirewall(ctx, newFW); err != nil {
							return fmt.Errorf("infra harden: create firewall %q: %w", firewallName, err)
						}
						status = "reconciled"
					}
					actions = append(actions, HardenAction{
						Dimension:   "firewall",
						Action:      "create_and_tag",
						Target:      firewallName,
						Status:      status,
						Description: fmt.Sprintf("create firewall %q with tag %q and ensure droplet has tag", firewallName, linkTag),
					})
				} else {
					// Firewall exists: check if droplet shares a tag with the firewall
					var commonTag string
					for _, ct := range currentTags {
						if slices.Contains(fw.Tags, ct) {
							commonTag = ct
							break
						}
					}

					if commonTag != "" {
						actions = append(actions, HardenAction{
							Dimension:   "firewall",
							Action:      "membership_via_tag",
							Target:      fw.Name,
							Status:      "noop",
							Description: fmt.Sprintf("droplet %d is already a member of firewall %q via tag %q", dropletID, fw.Name, commonTag),
						})
					} else {
						// Link using existing tag, firewall tag, or firewall name
						linkTag := firewallName
						if len(tags) > 0 && tags[0] != "" {
							linkTag = tags[0]
						} else if len(fw.Tags) > 0 {
							linkTag = fw.Tags[0]
						}

						status := "planned"
						if !dryRun {
							if !slices.Contains(currentTags, linkTag) {
								if err := client.TagDroplet(ctx, linkTag, dropletID); err != nil {
									return fmt.Errorf("infra harden: tag droplet %d: %w", dropletID, err)
								}
								currentTags = append(currentTags, linkTag)
							}
							if !slices.Contains(fw.Tags, linkTag) {
								if err := client.AddTagsToFirewall(ctx, fw.ID, []string{linkTag}); err != nil {
									return fmt.Errorf("infra harden: add tag %q to firewall %q: %w", linkTag, fw.Name, err)
								}
							}
							status = "reconciled"
						}
						actions = append(actions, HardenAction{
							Dimension:   "firewall",
							Action:      "membership_via_tag",
							Target:      fw.Name,
							Status:      status,
							Description: fmt.Sprintf("ensure membership in firewall %q via tag %q", fw.Name, linkTag),
						})
					}
				}
			}

			// 5. Reserved IP reconciliation
			if reservedIP {
				ips, err := client.ListReservedIPs(ctx)
				if err != nil {
					return fmt.Errorf("infra harden: list reserved IPs: %w", err)
				}

				var attachedIP string
				for _, r := range ips {
					if r.Droplet != nil && r.Droplet.ID == dropletID {
						attachedIP = r.IP
						break
					}
				}

				if attachedIP != "" {
					actions = append(actions, HardenAction{
						Dimension:   "reserved_ip",
						Action:      "ensure",
						Target:      attachedIP,
						Status:      "noop",
						Description: fmt.Sprintf("droplet %d already has reserved IP %s attached", dropletID, attachedIP),
					})
				} else {
					status := "planned"
					createdIP := "<new>"
					if !dryRun {
						newRip, err := client.CreateReservedIP(ctx, digitalocean.CreateReservedIPRequest{
							Region: droplet.Region.Slug,
						})
						if err != nil {
							return fmt.Errorf("infra harden: create reserved IP: %w", err)
						}
						createdIP = newRip.IP
						if err := client.AssignReservedIP(ctx, createdIP, dropletID); err != nil {
							return fmt.Errorf("infra harden: assign reserved IP %s to droplet %d: %w", createdIP, dropletID, err)
						}
						if _, err := client.WaitForReservedIPAssignment(ctx, createdIP, dropletID); err != nil {
							return fmt.Errorf("infra harden: verify reserved IP %s assigned to droplet %d: %w", createdIP, dropletID, err)
						}
						status = "reconciled"
					}
					actions = append(actions, HardenAction{
						Dimension:   "reserved_ip",
						Action:      "ensure",
						Target:      createdIP,
						Status:      status,
						Description: fmt.Sprintf("assign reserved IP to droplet %d in region %s", dropletID, droplet.Region.Slug),
					})
				}
			}

			// Determine overall reconciliation status
			allReconciled := true
			for _, a := range actions {
				if a.Status == "planned" {
					allReconciled = false
					break
				}
			}

			res := HardenResult{
				DropletID:  dropletID,
				DryRun:     dryRun,
				Reconciled: !dryRun && allReconciled,
				Actions:    actions,
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "harden plan for droplet %d:\n", dropletID)
				for _, a := range actions {
					fmt.Fprintf(cmd.OutOrStdout(), "  [%s] %s (%s)\n", a.Dimension, a.Description, a.Status)
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "hardened droplet %d:\n", dropletID)
				for _, a := range actions {
					fmt.Fprintf(cmd.OutOrStdout(), "  [%s] %s\n", a.Dimension, a.Description)
				}
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID (required)")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&projectName, "project", "", "Project name or ID to assign droplet to")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Tags to apply to droplet (repeat or comma-separated)")
	cmd.Flags().StringVar(&firewallName, "firewall", "", "Cloud firewall name to ensure membership via tag")
	cmd.Flags().BoolVar(&reservedIP, "reserved-ip", false, "Ensure a reserved IP is attached to the droplet")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview reconciliation plan without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
