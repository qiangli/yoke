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
	root.AddCommand(list, newPeopleAdd(opts), newPeopleSet(opts), newPeopleRm(opts))
	return root
}

func newPeopleList(opts []fleet.Option) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List human principals",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			people, _ := fleet.New(opts...).People()
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), people)
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
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

// osUserFlags parses the repeatable --os-user host=account binding.
func osUserFlags(vals []string) (map[string]string, error) {
	if len(vals) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, v := range vals {
		host, user, ok := strings.Cut(v, "=")
		if !ok || host == "" || user == "" {
			return nil, fmt.Errorf("principal: --os-user wants host=account, got %q", v)
		}
		out[host] = user
	}
	return out, nil
}

func newPeopleAdd(opts []fleet.Option) *cobra.Command {
	var p fleet.Person
	var osUsers []string
	c := &cobra.Command{
		Use:   "add <handle>",
		Short: "Add a human principal",
		Long: "Add a human principal.\n\n" +
			"Account names are recorded per host, never globally. Assuming the local\n" +
			"$USER exists on a remote machine is the most common way a cross-host\n" +
			"reach fails, so an unbound host makes `whois` say it is guessing.",
		Example:       "  bashy person add alice --display \"Alice\" --email alice@example.com --os-user host-a=alice --os-user host-b=al",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := osUserFlags(osUsers)
			if err != nil {
				return err
			}
			p.Handle, p.OSUsers = args[0], m
			cat := fleet.New(opts...)
			if err := cat.SavePerson(p); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p.Handle)
			return nil
		},
	}
	c.Flags().StringVar(&p.Display, "display", "", "human-facing name")
	c.Flags().StringVar(&p.Email, "email", "", "account email; authoritative identity when paired")
	c.Flags().StringVar(&p.DefaultOSUser, "default-os-user", "", "account name on hosts with no explicit binding")
	c.Flags().StringArrayVar(&p.Aliases, "alias", nil, "an additional name (repeatable)")
	c.Flags().StringArrayVar(&osUsers, "os-user", nil, "host=account binding (repeatable)")
	c.Flags().StringArrayVar(&p.Hosts, "host", nil, "a host this person owns (repeatable)")
	return c
}

func newPeopleSet(opts []fleet.Option) *cobra.Command {
	var display, email, defaultUser string
	var osUsers, addAlias []string
	c := &cobra.Command{
		Use:           "set <handle>",
		Short:         "Modify a human principal",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := fleet.New(opts...)
			p, ok := cat.Person(args[0])
			if !ok {
				return fmt.Errorf("principal: no person %q", args[0])
			}
			if cmd.Flags().Changed("display") {
				p.Display = display
			}
			if cmd.Flags().Changed("email") {
				p.Email = email
			}
			if cmd.Flags().Changed("default-os-user") {
				p.DefaultOSUser = defaultUser
			}
			if len(osUsers) > 0 {
				m, err := osUserFlags(osUsers)
				if err != nil {
					return err
				}
				if p.OSUsers == nil {
					p.OSUsers = map[string]string{}
				}
				for h, u := range m {
					p.OSUsers[h] = u
				}
			}
			p.Aliases = append(p.Aliases, addAlias...)
			if err := cat.SavePerson(p); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p.Handle)
			return nil
		},
	}
	c.Flags().StringVar(&display, "display", "", "human-facing name")
	c.Flags().StringVar(&email, "email", "", "account email")
	c.Flags().StringVar(&defaultUser, "default-os-user", "", "account name on hosts with no explicit binding")
	c.Flags().StringArrayVar(&osUsers, "os-user", nil, "host=account binding (repeatable)")
	c.Flags().StringArrayVar(&addAlias, "add-alias", nil, "add a name (repeatable)")
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
			if err := fleet.New(opts...).RemovePerson(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed person %s\n", args[0])
			return nil
		},
	}
}
