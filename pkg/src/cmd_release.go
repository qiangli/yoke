package src

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/spf13/cobra"
)

func newReleaseCmd() *cobra.Command {
	var user string
	var repair, asJSON bool
	var contract []string
	c := &cobra.Command{
		Use:   "release <app> <rev>",
		Short: "Tag a revision as the app's next semver release (patch=repair, minor=additive, major=contract)",
		Long: `release tags a revision of an app as its next semver release
(vX.Y.Z). The first release is v1.0.0. After that the bump follows the
change kind, derived by comparing the revision's declared surface (units,
routes, flags, arguments, stored properties) against the previous
release: --repair with the same surface is a patch, any addition is a
minor, and a removal or rename is refused unless every dropped element is
approved with --contract, which makes it a major. Unreleased revisions
are named by Go-style pseudo-versions; offerings publish releases only.
The principal recorded as the tagger comes from --user or
$BASHY_PRINCIPAL.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := resolveUser(user)
			if err != nil {
				return err
			}
			res, err := Release("", args[0], args[1], p, ReleaseOptions{Repair: repair, Contract: contract})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				b, err := json.MarshalIndent(res, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(out, string(b))
				return nil
			}
			was := ""
			if res.Previous != "" {
				was = " (was " + res.Previous + ")"
			}
			fmt.Fprintf(out, "released %s %s %s at %s%s\n", res.App, res.Version, res.Kind, shortSHA(res.Revision), was)
			if len(res.Added) > 0 {
				fmt.Fprintf(out, "  added: %s\n", strings.Join(res.Added, ", "))
			}
			if len(res.Contracted) > 0 {
				fmt.Fprintf(out, "  contracted: %s\n", strings.Join(res.Contracted, ", "))
			}
			return nil
		},
	}
	userFlag(c, &user)
	c.Flags().BoolVar(&repair, "repair", false, "the revision is a repair: same declared surface, patch bump")
	c.Flags().StringArrayVar(&contract, "contract", nil, "surface element approved for dropping (repeatable), e.g. \"property:title\"; makes the release a major")
	c.Flags().BoolVar(&asJSON, "json", false, "print the release as JSON")
	return c
}

func newReleasesCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "releases <app>",
		Short: "List an app's published releases, newest version first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			tags, err := Releases("", args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				if tags == nil {
					tags = []ReleaseTag{}
				}
				b, err := json.MarshalIndent(tags, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(out, string(b))
				return nil
			}
			if len(tags) == 0 {
				fmt.Fprintf(out, "(no releases of %s)\n", args[0])
				return nil
			}
			for _, t := range tags {
				kind := t.Kind
				if kind == "" {
					kind = "-"
				}
				fmt.Fprintf(out, "%s %s %s %s\n", t.Version, shortSHA(t.Revision), kind, t.When.Format("2006-01-02T15:04:05Z07:00"))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the releases as JSON")
	return c
}

func newVersionCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "version <app> [rev]",
		Short: "Name a revision: its release tag, or a pseudo-version when unreleased",
		Long: `version prints the version a revision of an app carries: the
semver release tag when it was released, else a Go-style pseudo-version
(v0.0.0-<UTC yyyymmddhhmmss>-<12-hex>). Without a rev it names the
principal's active revision (--user or $BASHY_PRINCIPAL), or HEAD when no
principal is set or none is active.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rev := ""
			if len(args) == 2 {
				rev = args[1]
			}
			if rev == "" {
				p := strings.TrimSpace(user)
				if p == "" {
					p = strings.TrimSpace(os.Getenv(principalEnv))
				}
				if p != "" {
					if store, err := state.OpenAt(Home(), args[0], p); err == nil {
						if st, err := store.Read(); err == nil {
							rev = st.ActiveRevision
						}
					}
				}
			}
			if rev == "" {
				rev = "HEAD"
			}
			v, _, err := Version("", args[0], rev)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), v)
			return nil
		},
	}
	userFlag(c, &user)
	return c
}
