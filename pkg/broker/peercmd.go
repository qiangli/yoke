package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/fleet"
)

// peersPath is the peers file the CLI edits (a seam for tests).
var peersPath = door.PeersPath

// peerMemberClient is the account member client the peer CLI uses (a seam for
// tests). The serving door keeps its own long-lived instance for offline use.
var peerMemberClient = func() (MemberAuthorizer, error) { return fleet.NewMemberClient("", "") }

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
	cmd.AddCommand(newPeerAddCmd(), newPeerSetCmd(), newPeerShareCmd(), newPeerRevokeCmd(), newPeerLimitCmd(), newPeerListCmd())
	return cmd
}

func newPeerAddCmd() *cobra.Command {
	var share, email, circle string
	var asJSON, dryRun bool
	c := &cobra.Command{
		Use:   "add NAME",
		Short: "mint (or rotate) a peer credential; prints it once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := peersPath()
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("email") || cmd.Flags().Changed("circle") {
				if email == "" {
					ps, loadErr := LoadPeerShares(path)
					if loadErr != nil {
						return loadErr
					}
					email = ps.Peers[args[0]].Email
				}
				if err := validatePeerMember(cmd.Context(), email, circle); err != nil {
					return err
				}
			}
			if dryRun {
				preview := map[string]any{"dry_run": true, "peer": args[0], "share": share, "email": email, "circle": circle}
				if asJSON {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(preview)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "would mint peer:%s email=%s circle=%s share=%s\n", args[0], email, circle, share)
				return nil
			}
			cred, err := MintPeerCredentialFor(path, args[0], share, email, circle, time.Now().UTC())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"peer": PeerPrincipalPrefix + args[0], "credential": cred, "email": email, "circle": circle})
			}
			fmt.Fprintln(cmd.OutOrStdout(), cred)
			return nil
		},
	}
	c.Flags().StringVar(&share, "share", "", "initial share policy: allow or deny (default deny; a rotation keeps the current policy)")
	c.Flags().StringVar(&email, "email", "", "bind this credential to an active mutual member email")
	c.Flags().StringVar(&circle, "circle", "", "also require that member's current circle")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "validate and show the change without writing")
	return c
}

func newPeerSetCmd() *cobra.Command {
	var email, circle string
	var asJSON, dryRun bool
	c := &cobra.Command{
		Use:   "set NAME --email EMAIL [--circle C]",
		Short: "set or clear a peer's member binding",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("email") && !cmd.Flags().Changed("circle") {
				return fmt.Errorf("llm peer set: give --email (use --email= to restore a legacy unbound peer) or --circle")
			}
			path, err := peersPath()
			if err != nil {
				return err
			}
			ps, err := LoadPeerShares(path)
			if err != nil {
				return err
			}
			rec, ok := ps.Peers[args[0]]
			if !ok {
				return fmt.Errorf("broker: no peer %q (mint one with `llm peer add %s`)", args[0], args[0])
			}
			if !cmd.Flags().Changed("email") {
				email = rec.Email
			}
			if !cmd.Flags().Changed("circle") {
				circle = rec.Circle
			}
			if email != "" {
				if err := validatePeerMember(cmd.Context(), email, circle); err != nil {
					return err
				}
			}
			result := map[string]any{"peer": PeerPrincipalPrefix + args[0], "email": email, "circle": circle}
			if dryRun {
				result["dry_run"] = true
			} else if err := SetPeerMember(path, args[0], email, circle, time.Now().UTC()); err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "would set peer:%s email=%s circle=%s\n", args[0], email, circle)
			}
			return nil
		},
	}
	c.Flags().StringVar(&email, "email", "", "active mutual member email (empty clears the binding)")
	c.Flags().StringVar(&circle, "circle", "", "also require the member's current circle (empty means exact email only)")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "validate and show the change without writing")
	return c
}

func validatePeerMember(ctx context.Context, email, circle string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return fmt.Errorf("llm peer: --circle requires a peer bound with --email")
	}
	c, err := peerMemberClient()
	if err != nil {
		return fmt.Errorf("llm peer: cannot check member %s: %w", email, err)
	}
	grants := []string{email}
	if circle = strings.TrimSpace(circle); circle != "" {
		grants = []string{circle}
	}
	d, err := c.Decide(ctx, email, grants)
	if err != nil {
		return fmt.Errorf("llm peer: cannot check member %s: %w", email, err)
	}
	// Writes fail closed while offline: the 24h exception is for serving an
	// already-issued credential, not creating a new authorization record.
	if d.Offline {
		return fmt.Errorf("llm peer: cannot change a member binding while the master is unreachable")
	}
	if !d.Allowed {
		return fmt.Errorf("llm peer: %s is not an active mutual member allowed by this grant", email)
	}
	return nil
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
			fmt.Fprintln(tw, "PEER\tSHARE\tEMAIL\tCIRCLE\tLIMITS\tCREATED")
			for _, n := range ps.Names() {
				lim := PeerLimits{}
				if ps.Peers[n].Limits != nil {
					lim = *ps.Peers[n].Limits
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", PeerPrincipalPrefix+n, ps.Peers[n].Share, ps.Peers[n].Email, ps.Peers[n].Circle, lim, ps.Peers[n].Created.Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
}
