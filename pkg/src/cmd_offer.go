package src

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newOfferCmd() *cobra.Command {
	var user string
	var to []string
	var all bool
	c := &cobra.Command{
		Use:   "offer <app> <rev>",
		Short: "Publish a revision of a feature as a host offering",
		Long: `offer publishes a released revision of a feature as a host
offering. Offerings publish tagged releases only: an unreleased revision
is refused until it is released with src release. The offering carries
only the source change plus origin principal, base revision, version, and
dependencies. Restricted offerings (--to) are invisible to
principals not listed; --all publishes to all principals on the host.
Eligible principals get one notice per offering revision.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			creator, err := resolveUser(user)
			if err != nil {
				return err
			}
			var targets []string
			for _, t := range to {
				for _, part := range strings.Split(t, ",") {
					if p := strings.TrimSpace(part); p != "" {
						targets = append(targets, p)
					}
				}
			}
			offering, err := Offer("", args[0], args[1], creator, OfferOptions{
				To:  targets,
				All: all,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "offered %s %s %s revision %s by %s\n",
				offering.App, offering.ID, offering.Version, shortSHA(offering.Revision), offering.Origin)
			return nil
		},
	}
	userFlag(c, &user)
	c.Flags().StringSliceVar(&to, "to", nil, "comma-separated list of eligible principals")
	c.Flags().BoolVar(&all, "all", false, "offer to all principals on the host")
	return c
}

func newOffersCmd() *cobra.Command {
	var user string
	var asJSON bool
	c := &cobra.Command{
		Use:   "offers <app>",
		Short: "List feature offerings visible to the principal",
		Long: `offers lists feature offerings visible to the principal for the
specified app. Restricted offerings not including the principal are hidden.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			offers, err := ListOffers("", args[0], p)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				b, err := json.MarshalIndent(offers, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(out, string(b))
				return nil
			}
			if len(offers) == 0 {
				fmt.Fprintf(out, "no offerings for %s visible to %s\n", args[0], p)
				return nil
			}
			for _, o := range offers {
				vis := "restricted"
				if o.All {
					vis = "all"
				}
				fmt.Fprintf(out, "offer %s %s %s rev %s by %s (%s)\n",
					o.App, o.ID, o.Version, shortSHA(o.Revision), o.Origin, vis)
			}
			return nil
		},
	}
	userFlag(c, &user)
	c.Flags().BoolVar(&asJSON, "json", false, "output offers as JSON")
	return c
}

func newAdoptCmd() *cobra.Command {
	var user string
	var preview bool
	c := &cobra.Command{
		Use:   "adopt <app> <offer>",
		Short: "Adopt an offering: preview first, or merge and activate",
		Long: `adopt accepts a published feature offering. With --preview, it
checks the revision out in an isolated workspace without moving the active
revision. Without --preview, it merges the offering into the principal's
version line and activates it through the activation gate. A conflict with
the adopter's own changes records a review version and leaves the active
revision unchanged.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			// No Gate: Adopt verifies the offering for this user's own pair
			// and gates on that fresh evidence (the offering's evidence binds
			// the origin's base).
			res, err := Adopt("", args[0], args[1], p, AdoptOptions{Preview: preview, Gate: overrideGate()})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if preview {
				active := "(none)"
				if res.Previous != "" {
					active = shortSHA(res.Previous)
				}
				fmt.Fprintf(out, "preview ready at %s\nopen it with: cd %s\nactive revision unchanged (%s)\n",
					res.PreviewDir, res.PreviewDir, active)
				return nil
			}
			prev := "(none)"
			if res.Previous != "" {
				prev = shortSHA(res.Previous)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "adopted %s for %s at %s (was %s)\n",
				res.OfferID, res.Principal, shortSHA(res.Revision), prev)
			return nil
		},
	}
	userFlag(c, &user)
	c.Flags().BoolVar(&preview, "preview", false, "preview in an isolated workspace without activating")
	return c
}

func newDeclineCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "decline <app> <offer>",
		Short: "Decline an offering and clear its notice",
		Long: `decline records the decision not to adopt an offering and clears
its pending notice from the principal's state.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			if err := Decline("", args[0], args[1], p); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "declined %s for %s\n", args[1], p)
			return nil
		},
	}
	userFlag(c, &user)
	return c
}
