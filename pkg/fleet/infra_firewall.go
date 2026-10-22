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

func newInfraFirewall() *cobra.Command {
	fwCmd := &cobra.Command{
		Use:   "firewall",
		Short: "Manage cloud firewalls (list, allow, deny)",
	}

	fwCmd.AddCommand(newInfraFirewallList())
	fwCmd.AddCommand(newInfraFirewallAllow())
	fwCmd.AddCommand(newInfraFirewallDeny())

	return fwCmd
}

func newInfraFirewallList() *cobra.Command {
	var name string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cloud firewalls",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := resolveDOClient()
			if err != nil {
				return err
			}
			ctx := context.Background()

			fws, err := client.ListFirewalls(ctx)
			if err != nil {
				return err
			}

			var filtered []digitalocean.Firewall
			for _, fw := range fws {
				if name != "" && fw.Name != name {
					continue
				}
				filtered = append(filtered, fw)
			}
			if filtered == nil {
				filtered = []digitalocean.Firewall{}
			}

			if asJSON {
				return writeJSON(cmd.OutOrStdout(), filtered)
			}

			for _, fw := range filtered {
				tags := strings.Join(fw.Tags, ",")
				if tags == "" {
					tags = "-"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tdroplets:%d\ttags:%s\trules:%d\n",
					fw.ID, fw.Name, len(fw.DropletIDs), tags, len(fw.InboundRules))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "filter by firewall name")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newInfraFirewallAllow() *cobra.Command {
	var name, tag, portProto, sourceCIDR string
	var dropletID, satID int
	var dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "allow",
		Short: "Allow firewall membership or rules (idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if name == "" {
				return errors.New("infra firewall allow: --name FW is required")
			}
			if tag == "" && dropletID <= 0 && portProto == "" {
				return errors.New("infra firewall allow: at least one of --tag, --droplet, or --port must be specified")
			}

			client, err := resolveDOClient()
			if err != nil {
				return err
			}
			ctx := context.Background()

			type allowResult struct {
				Firewall string   `json:"firewall"`
				Actions  []string `json:"actions"`
				DryRun   bool     `json:"dry_run"`
			}

			fw, err := client.FindFirewallByName(ctx, name)
			if err != nil {
				return fmt.Errorf("infra firewall allow: %w", err)
			}

			var port, proto string
			var sources []string
			if portProto != "" {
				var perr error
				port, proto, perr = digitalocean.ParsePortProtocol(portProto)
				if perr != nil {
					return fmt.Errorf("infra firewall allow: %w", perr)
				}
				if sourceCIDR != "" {
					sources = strings.Split(sourceCIDR, ",")
				} else {
					sources = []string{"0.0.0.0/0", "::/0"}
				}
			}

			// If firewall does not exist, create it with requested attributes
			if fw == nil {
				var plannedActions []string
				if tag != "" {
					plannedActions = append(plannedActions, fmt.Sprintf("add_tag:%s", tag))
				}
				if dropletID > 0 {
					plannedActions = append(plannedActions, fmt.Sprintf("add_droplet:%d", dropletID))
				}
				if portProto != "" {
					plannedActions = append(plannedActions, fmt.Sprintf("add_rule:%s/%s", proto, port))
				}
				plannedActions = append(plannedActions, "create_firewall")

				if dryRun {
					res := allowResult{Firewall: name, Actions: plannedActions, DryRun: true}
					if asJSON {
						return writeJSON(cmd.OutOrStdout(), res)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "[plan] create firewall %q with %s\n", name, strings.Join(plannedActions, ", "))
					return nil
				}

				newFW := &digitalocean.Firewall{
					Name:          name,
					OutboundRules: digitalocean.DefaultOutboundRules(),
				}
				if tag != "" {
					newFW.Tags = []string{tag}
				}
				if dropletID > 0 {
					newFW.DropletIDs = []int{dropletID}
				}
				if portProto != "" {
					newFW.InboundRules = []digitalocean.FirewallRule{
						{
							Protocol: proto,
							Ports:    port,
							Sources:  &digitalocean.RuleTargets{Addresses: sources},
						},
					}
				}
				created, err := client.CreateFirewall(ctx, newFW)
				if err != nil {
					return fmt.Errorf("infra firewall allow: create firewall: %w", err)
				}
				res := allowResult{Firewall: created.Name, Actions: plannedActions, DryRun: false}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created firewall %q with %s\n", created.Name, strings.Join(plannedActions, ", "))
				return nil
			}

			// Firewall exists: check membership and rule additions
			var actions []string
			needTag := tag != "" && !slices.Contains(fw.Tags, tag)
			if needTag {
				actions = append(actions, fmt.Sprintf("add_tag:%s", tag))
			}

			needDroplet := dropletID > 0 && !slices.Contains(fw.DropletIDs, dropletID)
			if needDroplet {
				actions = append(actions, fmt.Sprintf("add_droplet:%d", dropletID))
			}

			var needRule bool
			if portProto != "" {
				ruleExists := false
				for _, r := range fw.InboundRules {
					if r.Matches(proto, port, "") {
						// Check sources
						allCovered := true
						for _, s := range sources {
							if r.Sources == nil || !slices.Contains(r.Sources.Addresses, s) {
								allCovered = false
								break
							}
						}
						if allCovered {
							ruleExists = true
							break
						}
					}
				}
				if !ruleExists {
					needRule = true
					actions = append(actions, fmt.Sprintf("add_rule:%s/%s", proto, port))
				}
			}

			if len(actions) == 0 {
				res := allowResult{Firewall: fw.Name, Actions: []string{"already_allowed"}, DryRun: dryRun}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "firewall %q already configured\n", fw.Name)
				return nil
			}

			if dryRun {
				res := allowResult{Firewall: fw.Name, Actions: actions, DryRun: true}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[plan] firewall %q: %s\n", fw.Name, strings.Join(actions, ", "))
				return nil
			}

			if needTag {
				if err := client.AddTagsToFirewall(ctx, fw.ID, []string{tag}); err != nil {
					return fmt.Errorf("infra firewall allow: add tag %s: %w", tag, err)
				}
			}
			if needDroplet {
				if err := client.AddDropletsToFirewall(ctx, fw.ID, []int{dropletID}); err != nil {
					return fmt.Errorf("infra firewall allow: add droplet %d: %w", dropletID, err)
				}
			}
			if needRule {
				newRule := digitalocean.FirewallRule{
					Protocol: proto,
					Ports:    port,
					Sources:  &digitalocean.RuleTargets{Addresses: sources},
				}
				if err := client.AddRulesToFirewall(ctx, fw.ID, []digitalocean.FirewallRule{newRule}); err != nil {
					return fmt.Errorf("infra firewall allow: add rule: %w", err)
				}
			}

			res := allowResult{Firewall: fw.Name, Actions: actions, DryRun: false}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "firewall %q updated: %s\n", fw.Name, strings.Join(actions, ", "))
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Firewall name (required)")
	cmd.Flags().StringVar(&tag, "tag", "", "Tag to allow on the firewall")
	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID to allow on the firewall")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&portProto, "port", "", "Inbound rule port/protocol (e.g. 22/tcp, 443/tcp, icmp)")
	cmd.Flags().StringVar(&sourceCIDR, "source", "", "Source CIDR for inbound rule (default: 0.0.0.0/0,::/0)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview action without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newInfraFirewallDeny() *cobra.Command {
	var name, tag, portProto, sourceCIDR string
	var dropletID, satID int
	var dryRun, asJSON bool

	cmd := &cobra.Command{
		Use:   "deny",
		Short: "Deny firewall membership or rules (never deletes the firewall; idempotent)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dropletID == 0 && satID > 0 {
				dropletID = satID
			}
			if name == "" {
				return errors.New("infra firewall deny: --name FW is required")
			}
			if tag == "" && dropletID <= 0 && portProto == "" {
				return errors.New("infra firewall deny: at least one of --tag, --droplet, or --port must be specified")
			}

			client, err := resolveDOClient()
			if err != nil {
				return err
			}
			ctx := context.Background()

			type denyResult struct {
				Firewall string   `json:"firewall"`
				Actions  []string `json:"actions"`
				DryRun   bool     `json:"dry_run"`
			}

			fw, err := client.FindFirewallByName(ctx, name)
			if err != nil {
				return fmt.Errorf("infra firewall deny: %w", err)
			}
			if fw == nil {
				// Firewall doesn't exist: already denied (idempotent)
				res := denyResult{Firewall: name, Actions: []string{"already_denied"}, DryRun: dryRun}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "firewall %q does not exist (already denied)\n", name)
				return nil
			}

			var actions []string
			var removeRules []digitalocean.FirewallRule

			if portProto != "" {
				port, proto, perr := digitalocean.ParsePortProtocol(portProto)
				if perr != nil {
					return fmt.Errorf("infra firewall deny: %w", perr)
				}
				for _, r := range fw.InboundRules {
					if r.Matches(proto, port, sourceCIDR) {
						removeRules = append(removeRules, r)
						actions = append(actions, fmt.Sprintf("remove_rule:%s/%s", proto, port))
					}
				}
			}

			needRemoveTag := tag != "" && slices.Contains(fw.Tags, tag)
			if needRemoveTag {
				actions = append(actions, fmt.Sprintf("remove_tag:%s", tag))
			}

			needRemoveDroplet := dropletID > 0 && slices.Contains(fw.DropletIDs, dropletID)
			if needRemoveDroplet {
				actions = append(actions, fmt.Sprintf("remove_droplet:%d", dropletID))
			}

			if len(actions) == 0 {
				res := denyResult{Firewall: fw.Name, Actions: []string{"already_denied"}, DryRun: dryRun}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "firewall %q has no matching membership or rule to remove\n", fw.Name)
				return nil
			}

			if dryRun {
				res := denyResult{Firewall: fw.Name, Actions: actions, DryRun: true}
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "[plan] firewall %q: %s (firewall preserved)\n", fw.Name, strings.Join(actions, ", "))
				return nil
			}

			// Apply removals - note: NEVER deletes the firewall itself
			if len(removeRules) > 0 {
				if err := client.RemoveRulesFromFirewall(ctx, fw.ID, removeRules); err != nil {
					return fmt.Errorf("infra firewall deny: remove rules: %w", err)
				}
			}
			if needRemoveTag {
				if err := client.RemoveTagsFromFirewall(ctx, fw.ID, []string{tag}); err != nil {
					return fmt.Errorf("infra firewall deny: remove tag %s: %w", tag, err)
				}
			}
			if needRemoveDroplet {
				if err := client.RemoveDropletsFromFirewall(ctx, fw.ID, []int{dropletID}); err != nil {
					return fmt.Errorf("infra firewall deny: remove droplet %d: %w", dropletID, err)
				}
			}

			res := denyResult{Firewall: fw.Name, Actions: actions, DryRun: false}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "firewall %q updated: %s\n", fw.Name, strings.Join(actions, ", "))
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "Firewall name (required)")
	cmd.Flags().StringVar(&tag, "tag", "", "Tag to remove from the firewall")
	cmd.Flags().IntVar(&dropletID, "droplet", 0, "Droplet ID to remove from the firewall")
	cmd.Flags().IntVar(&satID, "sat", 0, "Satellite droplet ID (alias for --droplet)")
	cmd.Flags().StringVar(&portProto, "port", "", "Inbound rule port/protocol to remove")
	cmd.Flags().StringVar(&sourceCIDR, "source", "", "Source CIDR for rule matching")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview action without making changes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}
