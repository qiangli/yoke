package fleet

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// NewCommandsCmd builds the registered-command CRUD tree the embedding shell
// mounts UNDER its `commands` lister: list · show · schema · add · set · rm ·
// edit · verify. The lister itself stays the shell's (it merges every ring
// the binary knows); this tree is only the ring the operator writes.
//
// `sync` is deliberately absent: the record is serializable and an org ring
// can be mounted read-only through $BASHY_COMMANDS_PATH today, but there is
// no control-plane endpoint yet to pull one from.
func NewCommandsCmd(opts ...Option) *cobra.Command {
	root := newRoot("commands", "Registered commands — the ring bashy commands add writes",
		newCommandsList(opts),
		newShow(KindCommand, opts),
		newSchema(KindCommand),
		NewRetireCmd(KindCommand, opts...), NewUnretireCmd(KindCommand, opts...),
		newAdd(KindCommand, opts),
		newSet(KindCommand, opts),
		newRm(KindCommand, opts, (*Catalog).RemoveCommand),
		newEdit(KindCommand, opts, (*Catalog).MaterializeCommand),
		newVerify(KindCommand, opts, func(c *Catalog, n string) Check {
			return c.VerifyCommand(context.Background(), n)
		}),
	)
	root.Long = commandsLong
	return root
}

const commandsLong = `Registered commands: the ring ` + "`bashy commands add`" + ` writes, read beside every
shipped command by ` + "`bashy commands`" + `, ` + "`define`" + `, the atlas views, dry-run and
` + "`bashy agentic`" + `.

A record carries exactly ONE implementation and the atlas metadata every shipped
command has:

  exec       an argv template naming a program this host already has
             (--set exec.0=/path/to/prog --set exec.1=--flag; user args are
             appended, or replace a single {args} element)
  download   a pinned release bashy provisions on first use (--set
             download.github=owner/repo or download.url=TEMPLATE, plus
             download.version=TAG and download.sha256.<goos/goarch>=<hex>;
             no digest, no download)
  script     an inline body run as ` + "`bashy -c BODY NAME ARGS`" + ` (--set script=…;
             its effects are yours to declare: --set effects.0=read)

Any mode may declare an ARGUMENT SCHEMA — typed positionals, named flags,
enums and defaults — that the shell binds and validates BEFORE the body or
program runs, so a bad call fails on the argument, not inside the body:

  args.positionals.<i>   name, type (string|int|float|bool), required,
                         default, enum.<j>   (required ones first)
  args.flags.<i>         name (--name), shorthand (-x), type, required,
                         default, enum.<j>   (a bool flag alone means true)

  --set args.positionals.0.name=color --set args.positionals.0.required=true \
  --set args.positionals.0.enum.0=red --set args.positionals.0.enum.1=blue \
  --set args.flags.0.name=count --set args.flags.0.type=int

A record without args is untyped pass-through, exactly as before. An invalid
schema (a required positional after an optional one, a default outside its
enum, an unknown type) is refused when written and reported by verify.

Dispatch precedence is builtin -> applet -> verb -> REGISTERED -> PATH, in
every mode (--posix included): a registered name may shadow a PATH program,
never a command bashy ships — add refuses the collision, and a ring entry a
newer bashy has since claimed is listed as SHADOWED and skipped.

RING: shared = a read-only directory on $BASHY_COMMANDS_PATH; local = the
writable store under $BASHY_COMMANDS_DIR (default ~/.config/bashy/commands).
There is no embedded ring: bashy ships the mechanism, never a catalog. sync
from an org catalog is not available yet.`

type commandRow struct {
	RecordLifecycle
	Name     string   `json:"name"`
	Mode     string   `json:"mode"`
	Effects  []string `json:"effects"`
	Aliases  []string `json:"aliases,omitempty"`
	Ring     string   `json:"ring"`
	Hidden   bool     `json:"hidden,omitempty"`
	Shadowed string   `json:"shadowed_by,omitempty"`
	Synopsis string   `json:"synopsis,omitempty"`
}

func newCommandsList(opts []Option) *cobra.Command {
	var asJSON, all, retired bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List registered commands",
		Long:          commandsLong,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cat := New(opts...)
			cmds, errs := cat.Commands()
			shadows := cat.CommandShadows()
			rows := make([]commandRow, 0, len(cmds))
			for _, r := range cmds {
				if !MatchRetirement(r, retired) {
					continue
				}
				if r.Hidden && !all && !retired {
					continue
				}
				rows = append(rows, commandRow{
					RecordLifecycle: r.RecordLifecycle, Name: r.Name, Mode: r.Mode(), Effects: r.Effects, Aliases: r.Aliases,
					Ring: r.Ring.String(), Hidden: r.Hidden, Shadowed: shadows[r.Name], Synopsis: r.Synopsis,
				})
			}
			if asJSON {
				return writeJSON(cmd.OutOrStdout(), rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no registered commands — `bashy commands add NAME --set exec.0=/path/to/prog` (or --set script=…, --set download.url=…)")
				return reportParseErrs(cmd.ErrOrStderr(), errs)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tMODE\tEFFECTS\tRING\tSHADOWED-BY\tSYNOPSIS")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Mode, strings.Join(r.Effects, ","), r.Ring, dashIfEmpty(r.Shadowed), r.Synopsis)
			}
			tw.Flush()
			return reportParseErrs(cmd.ErrOrStderr(), errs)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().BoolVar(&retired, "retired", false, "show only retired entries")
	c.Flags().BoolVar(&all, "all", false, "include hidden entries")
	return c
}
