package broker

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
)

// peersPath is the peers file the CLI edits (a seam for tests).
var peersPath = door.PeersPath

func newPeerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer",
		Short: "per-peer door credentials and their share policy (deny by default)",
		Long: `A sphere peer reaches this door with its OWN credential, never the owner
token. ` + "`peer add NAME`" + ` mints one (printed once; only its SHA-256 is stored
beside the owner token) for principal peer:NAME. A minted peer is NOT shared:
requests are refused 403 until ` + "`peer share NAME allow`" + `. ` + "`peer share NAME deny`" + `
suspends it and ` + "`peer revoke NAME`" + ` deletes the credential (then 401). A shared
peer reaches only inference routes (including tool-command and --capability
runs) and /health.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newPeerAddCmd(), newPeerShareCmd(), newPeerRevokeCmd(), newPeerListCmd())
	return cmd
}

func newPeerAddCmd() *cobra.Command {
	var share string
	c := &cobra.Command{
		Use:   "add NAME",
		Short: "mint (or rotate) a peer credential; prints it once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := peersPath()
			if err != nil {
				return err
			}
			cred, err := MintPeerCredential(path, args[0], share, time.Now().UTC())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), cred)
			return nil
		},
	}
	c.Flags().StringVar(&share, "share", "", "initial share policy: allow or deny (default deny; a rotation keeps the current policy)")
	return c
}

func newPeerShareCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "share NAME allow|deny",
		Short: "set a peer's share policy",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := peersPath()
			if err != nil {
				return err
			}
			return SetPeerShare(path, args[0], args[1], time.Now().UTC())
		},
	}
}

func newPeerRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke NAME",
		Short: "delete a peer's credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := peersPath()
			if err != nil {
				return err
			}
			return RevokePeer(path, args[0])
		},
	}
}

func newPeerListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list peers and their share policy (never the credentials)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := peersPath()
			if err != nil {
				return err
			}
			ps, err := LoadPeerShares(path)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PEER\tSHARE\tCREATED")
			for _, n := range ps.Names() {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", PeerPrincipalPrefix+n, ps.Peers[n].Share, ps.Peers[n].Created.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
}
