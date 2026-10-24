package fleet

import (
	"bufio"
	"fmt"
	"net/mail"
	"strings"

	"github.com/spf13/cobra"
)

// NewSatelliteTransferCmd builds the single-step mutual-Members transfer.
func NewSatelliteTransferCmd() *cobra.Command {
	var master, token, to string
	var yes bool
	cmd := &cobra.Command{Use: "transfer <name|id> --to <email>", Short: "Transfer a satellite and everything it owns", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(to) == "" {
			return fmt.Errorf("satellite transfer needs --to")
		}
		addr, err := mail.ParseAddress(to)
		if err != nil || addr.Address != to {
			return fmt.Errorf("invalid --to email %q", to)
		}
		client, err := NewOwnerClient(master, token)
		if err != nil {
			return err
		}
		rows, err := client.Satellites(cmd.Context())
		if err != nil {
			return err
		}
		var sat *SatelliteEntry
		for i := range rows {
			if rows[i].ID == args[0] || rows[i].Name == args[0] {
				sat = &rows[i]
				break
			}
		}
		if sat == nil {
			return fmt.Errorf("not found or not yours")
		}
		name := sat.Name
		if name == "" {
			name = sat.ID
		}
		if !yes {
			fmt.Fprintf(cmd.OutOrStdout(), "Transfer %s and everything it owns to %s? [y/N] ", name, to)
			line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if err != nil && line == "" {
				return fmt.Errorf("transfer canceled")
			}
			answer := strings.ToLower(strings.TrimSpace(line))
			if answer != "y" && answer != "yes" {
				return fmt.Errorf("transfer canceled")
			}
		}
		res, err := client.Transfer(cmd.Context(), sat.ID, to)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "satellite: %s\nfrom: %s\nto: %s\ntransferred_at: %s\nnote: %s\n", res.SatelliteID, res.From, res.To, res.TransferredAt.Format("2006-01-02T15:04:05Z07:00"), res.Note)
		return nil
	}}
	cmd.Flags().StringVar(&master, "master", "", "master Cloudbox URL")
	cmd.Flags().StringVar(&token, "token", "", "owner Bearer token")
	cmd.Flags().StringVar(&to, "to", "", "recipient account email (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation")
	return cmd
}
