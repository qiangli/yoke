package fleet

import (
	"bufio"
	"fmt"
	"io"
	"net/mail"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// interactiveReader is an optional interface an io.Reader may implement to
// declare whether it behaves as an interactive terminal.
type interactiveReader interface {
	Interactive() bool
}

// stdinIsInteractive reports whether r is an interactive terminal input source.
func stdinIsInteractive(r io.Reader) bool {
	if r == nil {
		return false
	}
	if ir, ok := r.(interactiveReader); ok {
		return ir.Interactive()
	}
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// NewSatelliteTransferCmd builds the single-step mutual-Members transfer.
func NewSatelliteTransferCmd() *cobra.Command {
	var master, token, to string
	var yes, asJSON, noInput bool
	var acct accountFlag
	cmd := &cobra.Command{
		Use:           "transfer <name|id> --to <email>",
		Short:         "Transfer a satellite and everything it owns",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(to) == "" {
				return fmt.Errorf("satellite transfer needs --to")
			}
			addr, err := mail.ParseAddress(to)
			if err != nil || addr.Address != to {
				return fmt.Errorf("invalid --to email %q", to)
			}
			key := strings.TrimSpace(args[0])
			if key == "" {
				return fmt.Errorf("fleet: satellite transfer needs an ID or name")
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
			if !yes {
				if noInput {
					return fmt.Errorf("satellite transfer requires --yes with --no-input")
				}
				if !stdinIsInteractive(cmd.InOrStdin()) {
					return fmt.Errorf("satellite transfer requires --yes when stdin is not interactive")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Transfer %s and everything it owns to %s? [y/N] ", name, to)
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
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "satellite: %s\nfrom: %s\nto: %s\ntransferred_at: %s\nnote: %s\n", res.SatelliteID, res.From, res.To, res.TransferredAt.Format("2006-01-02T15:04:05Z07:00"), res.Note)
			return nil
		},
	}
	cmd.Flags().StringVar(&master, "master", "", "master Cloudbox URL")
	cmd.Flags().StringVar(&token, "token", "", "owner Bearer token")
	cmd.Flags().StringVar(&to, "to", "", "recipient account email (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip confirmation")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&noInput, "no-input", false, "never prompt; fail where a prompt would happen")
	acct.bind(cmd)
	return cmd
}
