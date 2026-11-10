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
runs) and /health. ` + "`peer limit NAME`" + ` caps a peer's tokens per UTC day, USD per
UTC month and requests per minute; over a cap the door answers 429 with the
limit, the usage and the reset time.`,
		SilenceUsage: true,
	}
	cmd.AddCommand(newPeerAddCmd(), newPeerShareCmd(), newPeerRevokeCmd(), newPeerLimitCmd(), newPeerListCmd())
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

func newPeerLimitCmd() *cobra.Command {
	var tokensDay uint64
	var usdMonth float64
	var rpm int
	var clear bool
	c := &cobra.Command{
		Use:   "limit NAME [--tokens-day N] [--usd-month USD] [--rpm N] [--clear]",
		Short: "cap a peer's tokens/day, USD/month and requests/minute (0 = unlimited)",
		Long: `Set the caps the door enforces for peer NAME. Only the flags given change;
0 removes that cap and --clear removes them all. Tokens are prompt + completion
tokens per UTC day; USD is the audit record's estimated cost per UTC month (an
unpriced model counts nothing); rpm counts admitted inference requests over the
trailing minute. A cap is checked before a request runs, so requests already in
flight may overshoot it once. The owner is never limited.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			if !clear && !f.Changed("tokens-day") && !f.Changed("usd-month") && !f.Changed("rpm") {
				return fmt.Errorf("llm peer limit: give --tokens-day, --usd-month, --rpm or --clear")
			}
			if clear && (f.Changed("tokens-day") || f.Changed("usd-month") || f.Changed("rpm")) {
				return fmt.Errorf("llm peer limit: --clear takes no other limit flag")
			}
			path, err := peersPath()
			if err != nil {
				return err
			}
			var lim PeerLimits
			if !clear {
				ps, err := LoadPeerShares(path)
				if err != nil {
					return err
				}
				if rec, ok := ps.Peers[args[0]]; ok && rec.Limits != nil {
					lim = *rec.Limits
				}
				if f.Changed("tokens-day") {
					lim.TokensDay = tokensDay
				}
				if f.Changed("usd-month") {
					lim.USDMonth = usdMonth
				}
				if f.Changed("rpm") {
					lim.RPM = rpm
				}
			}
			if err := SetPeerLimits(path, args[0], lim, time.Now().UTC()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s%s\t%s\n", PeerPrincipalPrefix, args[0], lim)
			return nil
		},
	}
	c.Flags().Uint64Var(&tokensDay, "tokens-day", 0, "prompt + completion tokens per UTC day (0 = unlimited)")
	c.Flags().Float64Var(&usdMonth, "usd-month", 0, "estimated USD per UTC month (0 = unlimited)")
	c.Flags().IntVar(&rpm, "rpm", 0, "inference requests per trailing minute (0 = unlimited)")
	c.Flags().BoolVar(&clear, "clear", false, "remove every cap")
	return c
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
			fmt.Fprintln(tw, "PEER\tSHARE\tLIMITS\tCREATED")
			for _, n := range ps.Names() {
				lim := PeerLimits{}
				if ps.Peers[n].Limits != nil {
					lim = *ps.Peers[n].Limits
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", PeerPrincipalPrefix+n, ps.Peers[n].Share, lim, ps.Peers[n].Created.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
}
