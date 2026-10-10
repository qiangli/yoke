package principal

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
)

// NewPeopleCmd builds the `people` verb tree — the human half of the
// principal namespace.
//
// It is deliberately a real, small noun rather than something derived from
// an account. An unpaired host still has humans on it, and `@alice` must
// resolve there. When the host is paired, the account email becomes the
// authoritative identity and slots into the same entry.
func NewPeopleCmd(opts ...fleet.Option) *cobra.Command {
	root := &cobra.Command{
		Use:           "person",
		Short:         "Human principals — who the names in prose refer to",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true

	list := newPeopleList(opts)
	root.RunE = list.RunE
	root.Flags().AddFlagSet(list.Flags())
	root.AddCommand(fleet.NewRetireCmd(fleet.KindPerson, opts...), fleet.NewUnretireCmd(fleet.KindPerson, opts...), list, fleet.NewShowCmd(fleet.KindPerson, opts), fleet.NewAddCmd(fleet.KindPerson, opts), fleet.NewSetCmd(fleet.KindPerson, opts),
		fleet.NewEditCmd(fleet.KindPerson, opts, func(c *fleet.Catalog, n string) (string, error) { return c.MaterializePerson(n) }),
		newPeopleRm(opts), fleet.NewSchemaCmd(fleet.KindPerson))
	return root
}

func newPeopleList(opts []fleet.Option) *cobra.Command {
	var asJSON, retired bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List human principals",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			people, _ := fleet.New(opts...).People()
			filtered := people[:0]
			for _, p := range people {
				if fleet.MatchRetirement(p, retired) {
					filtered = append(filtered, p)
				}
			}
			people = filtered
			if asJSON {
				return fleet.WriteListJSON(cmd.OutOrStdout(), fleet.KindPerson, fleet.BasicView(retired, false), people)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "HANDLE\tDISPLAY\tEMAIL\tACCOUNTS\tRING")
			for _, p := range people {
				var accts []string
				for h, u := range p.OSUsers {
					accts = append(accts, h+"="+u)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					p.Handle, p.Display, p.Email, strings.Join(accts, ","), p.Ring)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&retired, "retired", false, "show only retired entries")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

func newPeopleRm(opts []fleet.Option) *cobra.Command {
	return &cobra.Command{
		Use:           "rm <handle>",
		Short:         "Remove a human principal from the local store",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := fleet.New(opts...)
			cat.WarnUnretired(cmd.ErrOrStderr(), fleet.KindPerson, args[0])
			if err := cat.RemovePerson(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed person %s\n", args[0])
			return nil
		},
	}
}
