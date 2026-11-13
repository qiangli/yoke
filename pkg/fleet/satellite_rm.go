package fleet

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// NewSatelliteRmCmd builds `satellite rm <name|id>`: the owner removes a
// satellite row the master still carries (lost, never enrolled, or a dead
// endpoint) together with everything keyed on it (Sprint 630 S12).
//
// The master refuses a live satellite (enabled, active key, recent contact)
// unless --force is passed. --dry-run resolves and prints the target
// without calling the delete route.
func NewSatelliteRmCmd() *cobra.Command {
	var master, token string
	var yes, force, dryRun, asJSON, noInput bool
	var acct accountFlag
	cmd := &cobra.Command{
		Use:           "rm <name|id> [--force] [--dry-run] [--yes]",
		Short:         "Remove a satellite row and everything keyed on it",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := strings.TrimSpace(args[0])
			if key == "" {
				return fmt.Errorf("fleet: satellite rm needs an ID or name")
			}
			client, err := newOwnerClientFor(cmd.Context(), &acct, master, token)
			if err != nil {
				return err
			}
			rows, err := client.Satellites(cmd.Context())
			if err != nil {
				return err
			}
			sat, err := selectSatellite(rows, key)
			if err != nil {
				return err
			}
			name := sat.Name
			if name == "" {
				name = sat.ID
			}
			if dryRun {
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), map[string]any{
						"dry_run": true, "satellite_id": sat.ID, "name": sat.Name, "state": sat.State, "endpoint": sat.Endpoint, "force": force,
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "dry-run: would remove %s (%s, state %s, endpoint %s) force=%t\n", name, sat.ID, sat.State, orDash(sat.Endpoint), force)
				return nil
			}
			if !yes {
				if noInput {
					return fmt.Errorf("satellite rm requires --yes with --no-input")
				}
				if !stdinIsInteractive(cmd.InOrStdin()) {
					return fmt.Errorf("satellite rm requires --yes when stdin is not interactive")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Remove %s (%s, state %s) and everything keyed on it? [y/N] ", name, sat.ID, sat.State)
				line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("rm canceled")
				}
				answer := strings.ToLower(strings.TrimSpace(line))
				if answer != "y" && answer != "yes" {
					return fmt.Errorf("rm canceled")
				}
			}
			res, err := client.Delete(cmd.Context(), sat.ID, force)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed: %s\nname: %s\nstate: %s\nreleased_hosts: %d\nforced: %t\ndeleted_at: %s\n",
				res.SatelliteID, res.Name, res.State, len(res.ReleasedHosts), res.Forced, res.DeletedAt.Format("2006-01-02T15:04:05Z07:00"))
			return nil
		},
	}
	cmd.Flags().StringVar(&master, "master", "", "master Cloudbox URL")
	cmd.Flags().StringVar(&token, "token", "", "owner Bearer token")
	cmd.Flags().BoolVar(&force, "force", false, "remove a live satellite too (the master refuses one with recent contact otherwise)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "resolve and print the target; call nothing")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&noInput, "no-input", false, "never prompt; fail where a prompt would happen")
	acct.bind(cmd)
	return cmd
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
