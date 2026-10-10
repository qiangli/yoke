package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	// Registering a resourcekind is registering a coord kind, and guarding a
	// removal reads live claims through the registry-derived providers: both
	// need the fleetkinds wiring active in any process serving this tree.
	"github.com/qiangli/yoke/pkg/fleet"
	_ "github.com/qiangli/yoke/pkg/fleet/fleetkinds"
	"github.com/qiangli/yoke/pkg/policy/coord"
)

// newRegistryCmds builds the fleet-registry half of `bashy resource`: the
// operator's names for things agents must not use at the same time. This is
// the file to the fleet's binary — add/rm/list/show/set manage the records,
// and claims on them go through coord under the kind each record declares.
//
// Removing a resource never destroys the underlying thing: it drops only the
// record, and a live claim on it refuses the removal first.
func newRegistryCmds() []*cobra.Command {
	return []*cobra.Command{
		newResourceAdd(),
		newResourceRm(),
		newResourceList(),
		newResourceShow(),
		newResourceSet(),
		newResourceKindCmd(),
	}
}

func newResourceAdd() *cobra.Command {
	var kind, title, notes, ttl, mode string
	var aliases, guards []string
	c := &cobra.Command{
		Use:   "add NAME --kind KIND [--title T] [--alias A]... [--guard PREFIX]... [MEMBER...]",
		Short: "Register a resource: a name for something agents must not share",
		Long: "Register a resource: a name for something agents must not share.\n\n" +
			"A resource declares which claim kind it is held under (--kind: a builtin\n" +
			"like path, or a `resource kind add` name) and what those claims cover\n" +
			"(the MEMBER positionals). A claim on the resource then conflicts exactly\n" +
			"as a claim of its kind over its members would. --guard prefixes are stored\n" +
			"verbatim for another lane to enforce; this command never interprets them.",
		Example: "  bashy resource add gpu0 --kind gpunode --title \"GPU node 0\" gpu:0\n" +
			"  bashy resource add stagedir --kind path --alias stage /w/stage",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := fleet.New()
			rec := fleet.Resource{
				Name: args[0], Kind: kind, Members: append([]string(nil), args[1:]...),
				Title: title, Notes: notes, Aliases: aliases, TTL: ttl, Mode: mode, Guard: guards,
			}
			if err := rec.Validate(); err != nil {
				return err
			}
			for _, n := range append([]string{rec.Name}, rec.Aliases...) {
				if holder, ok := cat.Resource(n); ok {
					return fmt.Errorf("resource: %q already belongs to resource %q (resource set %s ... to change it)", n, holder.Name, holder.Name)
				}
			}
			if err := cat.SaveResource(rec); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (kind %s)\n", rec.Name, rec.Kind)
			return nil
		},
	}
	c.Flags().StringVar(&kind, "kind", "", "claim kind the resource is held under (required): a builtin like path, or a resourcekind name")
	c.Flags().StringVar(&title, "title", "", "human-facing label")
	c.Flags().StringVar(&notes, "notes", "", "free-form notes")
	c.Flags().StringVar(&ttl, "ttl", "", "Go duration overriding the kind's claim TTL (e.g. 2h)")
	c.Flags().StringVar(&mode, "mode", "", "narrow the kind's claim modes to one: lease, attached or announce")
	c.Flags().StringArrayVar(&aliases, "alias", nil, "alternate accepted name (repeatable)")
	c.Flags().StringArrayVar(&guards, "guard", nil, "argv prefix another lane enforces (repeatable; stored verbatim)")
	return c
}

func newResourceRm() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "rm NAME",
		Short: "Drop a resource record (never the underlying thing)",
		Long: "Drop a resource record. Only the record goes: the underlying thing is\n" +
			"never touched. A live claim on the resource refuses first, unless --force.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cat := fleet.New()
			if _, ok := cat.Resource(name); !ok {
				return fmt.Errorf("resource: no resource %q", name)
			}
			if !force {
				if err := guardResource(cmd, name); err != nil {
					return err
				}
			}
			if err := cat.RemoveResource(name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed resource %s\n", name)
			return nil
		},
	}
	c.Flags().BoolVar(&force, "force", false, "drop the record even while a live claim holds it (the underlying thing is still never destroyed)")
	return c
}

// guardResource refuses while someone else holds a live claim on the
// resource. The holder passes; a lapsed claim is no claim. The refusal is
// the coord conflict unchanged, plus the way past it.
func guardResource(cmd *cobra.Command, name string) error {
	err := coord.Guard(cmd.Context(), coord.Self(), coord.Use{Kind: fleet.KindResource, Name: name})
	if err == nil {
		return nil
	}
	var conflict *coord.Conflict
	if errors.As(err, &conflict) {
		return fmt.Errorf("%w\nresource rm %s --force drops the record anyway (the underlying thing is never destroyed)", err, name)
	}
	return err
}

func newResourceList() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List registered resources",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			all, errs := fleet.New().Resources()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(all)
			}
			if len(all) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no registered resources — `bashy resource add NAME --kind KIND MEMBER...`")
				return reportResourceErrs(cmd, errs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tMEMBERS\tTITLE\tRING")
			for _, r := range all {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Kind, strings.Join(r.Members, ","), dashOrEmpty(r.Title), r.Ring)
			}
			tw.Flush()
			return reportResourceErrs(cmd, errs)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

func newResourceShow() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:           "show NAME",
		Short:         "Print a resource's record",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, ok := fleet.New().Resource(args[0])
			if !ok {
				return fmt.Errorf("resource: no resource %q", args[0])
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rec)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "name:    %s\n", rec.Name)
			fmt.Fprintf(out, "kind:    %s\n", rec.Kind)
			fmt.Fprintf(out, "members: %s\n", strings.Join(rec.Members, " "))
			if rec.Title != "" {
				fmt.Fprintf(out, "title:   %s\n", rec.Title)
			}
			if rec.Notes != "" {
				fmt.Fprintf(out, "notes:   %s\n", rec.Notes)
			}
			if len(rec.Aliases) > 0 {
				fmt.Fprintf(out, "aliases: %s\n", strings.Join(rec.Aliases, " "))
			}
			if rec.TTL != "" {
				fmt.Fprintf(out, "ttl:     %s\n", rec.TTL)
			}
			if rec.Mode != "" {
				fmt.Fprintf(out, "mode:    %s\n", rec.Mode)
			}
			if len(rec.Guard) > 0 {
				fmt.Fprintf(out, "guard:   %s\n", strings.Join(rec.Guard, " "))
			}
			fmt.Fprintf(out, "ring:    %s\n", rec.Ring)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

func newResourceSet() *cobra.Command {
	var kind, title, notes, ttl, mode string
	var members, rmMembers, addAliases, rmAliases, guards, rmGuards []string
	c := &cobra.Command{
		Use:           "set NAME [--kind K] [--title T] [--member M]... [--rm-member M]... [--add-alias A]... [--rm-alias A]... [--guard P]... [--rm-guard P]...",
		Short:         "Modify a resource's record",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := fleet.New()
			rec, ok := cat.Resource(args[0])
			if !ok {
				return fmt.Errorf("resource: no resource %q", args[0])
			}
			// Rewriting the entry rewrites what live claims cover: a live
			// claim held by someone else refuses first, unchanged.
			if err := coord.Guard(cmd.Context(), coord.Self(), coord.Use{Kind: fleet.KindResource, Name: rec.Name}); err != nil {
				return err
			}
			if cmd.Flags().Changed("kind") {
				rec.Kind = kind
			}
			if cmd.Flags().Changed("title") {
				rec.Title = title
			}
			if cmd.Flags().Changed("notes") {
				rec.Notes = notes
			}
			if cmd.Flags().Changed("ttl") {
				rec.TTL = ttl
			}
			if cmd.Flags().Changed("mode") {
				rec.Mode = mode
			}
			rec.Members = mergeResourceValues(rec.Members, members, rmMembers)
			rec.Aliases = mergeResourceValues(rec.Aliases, addAliases, rmAliases)
			rec.Guard = mergeResourceValues(rec.Guard, guards, rmGuards)
			if cmd.Flags().Changed("kind") || cmd.Flags().Changed("title") || cmd.Flags().Changed("notes") ||
				cmd.Flags().Changed("ttl") || cmd.Flags().Changed("mode") || len(members) > 0 || len(rmMembers) > 0 ||
				len(addAliases) > 0 || len(rmAliases) > 0 || len(guards) > 0 || len(rmGuards) > 0 {
				for _, n := range append([]string{rec.Name}, rec.Aliases...) {
					if holder, ok := cat.Resource(n); ok && holder.Name != rec.Name {
						return fmt.Errorf("resource: %q already belongs to resource %q", n, holder.Name)
					}
				}
			}
			if err := rec.Validate(); err != nil {
				return err
			}
			if err := cat.SaveResource(rec); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), rec.Name)
			return nil
		},
	}
	c.Flags().StringVar(&kind, "kind", "", "claim kind the resource is held under")
	c.Flags().StringVar(&title, "title", "", "human-facing label")
	c.Flags().StringVar(&notes, "notes", "", "free-form notes")
	c.Flags().StringVar(&ttl, "ttl", "", "Go duration overriding the kind's claim TTL (empty clears it)")
	c.Flags().StringVar(&mode, "mode", "", "narrow the kind's claim modes to one (empty clears it)")
	c.Flags().StringArrayVar(&members, "member", nil, "cover an additional member (repeatable)")
	c.Flags().StringArrayVar(&rmMembers, "rm-member", nil, "stop covering a member (repeatable)")
	c.Flags().StringArrayVar(&addAliases, "add-alias", nil, "add an alternate accepted name (repeatable)")
	c.Flags().StringArrayVar(&rmAliases, "rm-alias", nil, "drop an alternate accepted name (repeatable)")
	c.Flags().StringArrayVar(&guards, "guard", nil, "store an argv prefix another lane enforces (repeatable)")
	c.Flags().StringArrayVar(&rmGuards, "rm-guard", nil, "drop a stored guard prefix (repeatable)")
	return c
}

// mergeResourceValues applies an append/remove pair to a record list,
// dropping empties and duplicates while keeping first-seen order.
func mergeResourceValues(cur, add, rm []string) []string {
	drop := map[string]bool{}
	for _, v := range rm {
		drop[v] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range append(append([]string(nil), cur...), add...) {
		if v == "" || drop[v] || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

func dashOrEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func reportResourceErrs(cmd *cobra.Command, errs []error) error {
	for _, err := range errs {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning:", err)
	}
	return nil
}

// --- resource kinds --------------------------------------------------------

// newResourceKindCmd builds `bashy resource kind`: the user-claim-kind
// records. A kind declares the match rule, domain, TTL, modes and hook
// commands one class of claims collides under; resourcekind records register
// coord kinds at load, so user kinds work wherever builtin kinds do.
func newResourceKindCmd() *cobra.Command {
	c := &cobra.Command{
		Use:           "kind",
		Short:         "User claim kinds: the match rules resources are held under",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	c.AddCommand(newResourceKindAdd(), newResourceKindRm(), newResourceKindList(), newResourceKindShow())
	return c
}

func newResourceKindAdd() *cobra.Command {
	var match, domain, ttl, resolve, probe string
	var modes []string
	c := &cobra.Command{
		Use:   "add NAME --match M [--domain D] [--ttl T] [--mode MODE]... [--resolve CMD] [--probe CMD]",
		Short: "Declare a user claim kind",
		Long: "Declare a user claim kind.\n\n" +
			"--match is how claims of the kind collide: name (same name), member\n" +
			"(a shared member string), or path (equal or containing paths).\n" +
			"--domain scopes collisions; kinds in different domains never conflict.\n" +
			"--resolve and --probe name REGISTERED commands: a hook naming nothing\n" +
			"registered is refused when it runs, never from PATH. The resolve command\n" +
			"turns a name into members (one per output line); the probe command tests\n" +
			"a name (exit 0 free, 1 busy, anything else unknown).",
		Example:       "  bashy resource kind add gpunode --match member --domain gpu --resolve gpu-members --probe gpu-probe",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := fleet.New()
			name := args[0]
			rec := fleet.ResourceKind{
				Name: name, Match: match, Domain: domain, TTL: ttl,
				Modes: modes, Resolve: resolve, Probe: probe,
			}
			if rec.Match == "" {
				return fmt.Errorf("resource: kind %q: --match is required (name, member or path)", name)
			}
			if err := rec.Validate(); err != nil {
				return err
			}
			// A kind name must not hijack a kind something else owns. Updating
			// the record itself is fine; taking a builtin or fleet-noun kind
			// is refused.
			if _, ok := cat.ResourceKind(name); !ok {
				if k, ok := coord.LookupKind(name); ok {
					return fmt.Errorf("resource: kind %q already names a registered claim kind (match %s); pick another name", name, k.Match)
				}
			}
			if err := cat.SaveResourceKind(rec); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (match %s)\n", rec.Name, rec.Match)
			return nil
		},
	}
	c.Flags().StringVar(&match, "match", "", "collision rule (required): name, member or path")
	c.Flags().StringVar(&domain, "domain", "", "collision scope (empty means the kind's own name)")
	c.Flags().StringVar(&ttl, "ttl", "", "Go duration overriding the package claim TTL for lease-mode claims")
	c.Flags().StringArrayVar(&modes, "mode", nil, "permitted claim mode (repeatable: lease, attached, announce; empty permits all)")
	c.Flags().StringVar(&resolve, "resolve", "", "registered command turning a name into members")
	c.Flags().StringVar(&probe, "probe", "", "registered command testing a name (exit 0 free, 1 busy, else unknown)")
	return c
}

func newResourceKindRm() *cobra.Command {
	return &cobra.Command{
		Use:           "rm NAME",
		Short:         "Drop a user claim kind",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := fleet.New().RemoveResourceKind(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed resource kind %s\n", args[0])
			return nil
		},
	}
}

func newResourceKindList() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List user claim kinds",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			all, errs := fleet.New().ResourceKinds()
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(all)
			}
			if len(all) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no user claim kinds — `bashy resource kind add NAME --match member`")
				return reportResourceErrs(cmd, errs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMATCH\tDOMAIN\tMODES\tRING")
			for _, r := range all {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, dashOrEmpty(r.Match), dashOrEmpty(r.Domain), strings.Join(r.Modes, ","), r.Ring)
			}
			tw.Flush()
			return reportResourceErrs(cmd, errs)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

func newResourceKindShow() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:           "show NAME",
		Short:         "Print a user claim kind's record",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rec, ok := fleet.New().ResourceKind(args[0])
			if !ok {
				return fmt.Errorf("resource: no resource kind %q", args[0])
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rec)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "name:    %s\n", rec.Name)
			fmt.Fprintf(out, "match:   %s\n", rec.Match)
			fmt.Fprintf(out, "domain:  %s\n", rec.Domain)
			if rec.TTL != "" {
				fmt.Fprintf(out, "ttl:     %s\n", rec.TTL)
			}
			if len(rec.Modes) > 0 {
				fmt.Fprintf(out, "modes:   %s\n", strings.Join(rec.Modes, " "))
			}
			if rec.Resolve != "" {
				fmt.Fprintf(out, "resolve: %s\n", rec.Resolve)
			}
			if rec.Probe != "" {
				fmt.Fprintf(out, "probe:   %s\n", rec.Probe)
			}
			fmt.Fprintf(out, "ring:    %s\n", rec.Ring)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}
