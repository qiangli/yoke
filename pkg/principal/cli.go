package principal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Exit codes. 0 resolved, 1 not found, 2 usage — the repo convention — plus
// 3 for an ambiguous name, which is neither "missing" nor a usage error and
// which a caller must handle by qualifying the query.
const (
	ExitResolved  = 0
	ExitNotFound  = 1
	ExitUsage     = 2
	ExitAmbiguous = 3
)

// ExitCode maps a whois error to that convention.
func ExitCode(err error) int {
	if err == nil {
		return ExitResolved
	}
	var amb ambiguousError
	if ok := asAmbiguous(err, &amb); ok {
		return ExitAmbiguous
	}
	return assetring.ExitCode(err)
}

type ambiguousError struct {
	query string
	kinds []string
}

func (e ambiguousError) Error() string {
	return "principal: " + strconv.Quote(e.query) + " " + ambiguityHint(e.query, e.kinds)
}

func asAmbiguous(err error, out *ambiguousError) bool {
	e, ok := err.(ambiguousError)
	if ok {
		*out = e
	}
	return ok
}

// NewWhoisCmd builds the `whois` verb.
//
// The verb is a front door on the resolver library. The library is the
// load-bearing half: kb, meet, weave, and the mention linter all call it, so
// there is exactly one answer to "who is 007" on this host. The verb exists
// because a human — or an agent — sometimes needs to ask directly.
func NewWhoisCmd(opts ...fleet.Option) *cobra.Command {
	var asJSON, reach, check bool
	var method string
	c := &cobra.Command{
		Use:   "whois <name>|<kind>:<name>|-",
		Short: "Resolve a name to a principal and say how to reach it",
		Long: "Resolve a name to a principal and say how to reach it.\n\n" +
			"Names resolve across people, agents, tools, models, and hosts. A bare\n" +
			"name that matches two kinds is ambiguous (exit 3); qualify it as\n" +
			"`host:name` or `agent:name`.\n\n" +
			"Every answer carries a ranked ladder of contact methods, live ones\n" +
			"first. The ladder is never collapsed to a single choice: a laptop on\n" +
			"the LAN one minute is remote the next, and a cached \"best\" method is\n" +
			"guaranteed wrong after it roams.",
		Example: "  bashy whois 007\n" +
			"  bashy whois host-a\n" +
			"  ssh $(bashy whois host-a --reach --method ssh)\n" +
			"  bashy kb show notes | bashy whois --check -",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			r := NewResolver(fleet.New(opts...), DefaultEnv())

			if check {
				return runCheck(cmd, r, args[0], asJSON)
			}

			ans := r.Resolve(args[0])
			if !ans.Resolved {
				if asJSON {
					return writeJSON(cmd.OutOrStdout(), ans)
				}
				return fmt.Errorf("principal: %q names nothing on this host", args[0])
			}
			if ans.Ambiguous() && !asJSON {
				return ambiguousError{query: args[0], kinds: ans.Kinds()}
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), ans)
			}
			if reach {
				best, ok := pickContact(ans.Matches[0], method)
				if !ok {
					if method != "" {
						return fmt.Errorf("principal: %q has no live %q contact from here", args[0], method)
					}
					return fmt.Errorf("principal: %q has no live contact method from here", args[0])
				}
				fmt.Fprintln(cmd.OutOrStdout(), reachArg(best))
				return nil
			}
			printResolution(cmd.OutOrStdout(), ans.Matches[0])
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the bashy-whois-v1 envelope")
	c.Flags().BoolVar(&reach, "reach", false, "print only the best live contact address, for scripting")
	c.Flags().BoolVar(&check, "check", false, "lint @mentions in a file (or - for stdin)")
	c.Flags().StringVar(&method, "method", "", "with --reach, require this contact method (ssh, mdns, cli, ...)")
	c.SilenceUsage = true
	return c
}

// pickContact returns the best live contact, optionally restricted to one
// method. Restriction matters: the best contact for a host on the LAN is its
// mDNS name, which `ssh` cannot use as-is, so a caller that wants an ssh
// target must say so.
func pickContact(r Resolution, method string) (Contact, bool) {
	for _, c := range r.Contacts {
		if !c.Live {
			continue
		}
		if method == "" || c.Method == method {
			return c, true
		}
	}
	return Contact{}, false
}

// reachArg renders a contact for a shell to consume. An ssh contact prints as
// user@host, because that is what `ssh` takes; the scheme is display only.
func reachArg(c Contact) string {
	if c.Method == "ssh" {
		return strings.TrimPrefix(c.Address, "ssh://")
	}
	return c.Address
}

func printResolution(w io.Writer, r Resolution) {
	head := r.Name
	if r.Display != "" && r.Display != r.Name {
		head += "  " + r.Display
	}
	fmt.Fprintf(w, "%s  (%s", head, r.Kind)
	if r.Summary != "" {
		fmt.Fprintf(w, ", %s", r.Summary)
	}
	if r.Owner != "" && r.Owner != LocalOwner {
		fmt.Fprintf(w, ", owner %s", r.Owner)
	}
	fmt.Fprintln(w, ")")
	if len(r.Aliases) > 0 {
		fmt.Fprintf(w, "aliases: %s\n", strings.Join(r.Aliases, " "))
	}
	if r.Canonical != "" {
		fmt.Fprintf(w, "%-14s %s\n", "ref:", r.Canonical)
	}
	if r.Source != "" {
		s := r.Source
		if r.Confidence != "" {
			s += " (" + string(r.Confidence) + ")"
		}
		fmt.Fprintf(w, "%-14s %s\n", "source:", s)
	}
	for _, f := range r.Facts {
		fmt.Fprintf(w, "%-14s %s\n", f[0]+":", f[1])
	}
	if len(r.Contacts) == 0 {
		return
	}
	fmt.Fprintln(w, "contacts (ranked):")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, c := range r.Contacts {
		state := "ok"
		if !c.Live {
			state = "--"
		}
		fmt.Fprintf(tw, "  %d.\t%s\t%s\t%s\t%s\t%s\n",
			i+1, c.Method, c.Address, state, c.Source, c.Confidence)
		if c.Why != "" {
			fmt.Fprintf(tw, "  \t\t%s\t\t\t\n", "— "+c.Why)
		}
	}
	tw.Flush()
}

// runCheck lints the @mentions in a file or on stdin.
func runCheck(cmd *cobra.Command, r *Resolver, path string, asJSON bool) error {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	bad := r.CheckMentions(string(data))
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), bad)
	}
	for _, u := range bad {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s %s\n", u.Raw, u.Why)
	}
	// Warn, never fail: an unresolvable mention may live on a host that has
	// not synced. Failing here would put the network on the read path.
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
