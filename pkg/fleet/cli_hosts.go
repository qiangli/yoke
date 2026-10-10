package fleet

import (
	"fmt"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// NewHostsCmd builds the `host` verb tree: the static reach aliases an
// operator writes for machines discovery cannot find. The bare noun is its
// `list` verb; every other verb is built from the kind table.
func NewHostsCmd(opts ...Option) *cobra.Command {
	return newRoot("host", "Static reach aliases — where a machine answers and who to be there",
		newHostsList(opts),
		newShow(KindHost, opts),
		newSchema(KindHost),
		NewRetireCmd(KindHost, opts...), NewUnretireCmd(KindHost, opts...),
		newAdd(KindHost, opts),
		newSet(KindHost, opts),
		newRm(KindHost, opts, (*Catalog).RemoveHost),
		newEdit(KindHost, opts, (*Catalog).MaterializeHost),
	)
}

func newHostsList(opts []Option) *cobra.Command {
	var asJSON, retired bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List static host aliases",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			all, errs := New(opts...).Hosts()
			hosts := make([]Host, 0, len(all))
			for _, h := range all {
				if MatchRetirement(h, retired) {
					hosts = append(hosts, h)
				}
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), hosts)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tADDRESS\tUSER\tPORT\tRING")
			for _, h := range hosts {
				port := "-"
				if h.SSHPort != 0 {
					port = strconv.Itoa(h.SSHPort)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, dashIfEmpty(h.Address), dashIfEmpty(h.SSHUser), port, h.Ring)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			return reportParseErrs(cmd.ErrOrStderr(), errs)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().BoolVar(&retired, "retired", false, "show only retired entries")
	return c
}
