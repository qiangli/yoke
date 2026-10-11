package fleet

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/qiangli/yoke/pkg/assetring"
)

// NewServiceCmd builds the unit-definition tree the embedding shell mounts as
// `bashy service`: list · show · schema · add · set · rm · edit · enable ·
// disable · retire · unretire. These verbs own the DECLARATION and its
// desired state only; the runtime verbs (start · stop · restart · status ·
// logs · run) and the monitor are S16's, over the same records.
//
// Every verb here is generic over the record: there is no branch on a unit's
// name, so a built-in seed, an internal capability and a fresh third-party
// unit go through the same code.
func NewServiceCmd(opts ...Option) *cobra.Command {
	root := newRoot("service", "Service units — the daemons bashy supervisord keeps running",
		newServiceList(opts),
		newShow(KindService, opts),
		newSchema(KindService),
		NewRetireCmd(KindService, opts...), NewUnretireCmd(KindService, opts...),
		newAdd(KindService, opts),
		newSet(KindService, opts),
		newRm(KindService, opts, (*Catalog).RemoveService),
		newEdit(KindService, opts, (*Catalog).MaterializeService),
		newServiceToggle(true, opts),
		newServiceToggle(false, opts),
	)
	root.Long = serviceLong
	return root
}

const serviceLong = `Service units: one YAML declaration per daemon, built-in or third party.

A unit names its provider — process (a foreground exec the monitor owns) or
internal (a capability registered in-process by its owning subsystem) — and
declares, as data, everything the runtime needs: ports, health, restart and
start limits, requires, env and vault secrets, settings, pinned download,
app/mesh exposure and the desired state (enabled). Built-in units are
compiled-in seeds; ` + "`set`" + ` overrides one with a sparse local overlay and ` + "`rm`" + `
of that overlay restores the seed.

  bashy service show meet --yaml | bashy service add -f -    # identity
  bashy service set meet --set ports.0.port=8700              # operator override
  bashy service enable meet                                   # desired state

An enabled unit's ports may not overlap another enabled unit's: add, set and
enable refuse the write and name the holder.

RING: embedded = the built-in units; shared = read-only dirs on
$BASHY_SERVICES_PATH; local = $BASHY_SERVICES_DIR (default
~/.config/bashy/services).`

func serviceRecord() *recordSpec {
	return newRecordSpec(typedRecord[Service]{
		Get:  (*Catalog).Service,
		Name: func(s *Service) *string { return &s.Name },
		Ring: func(s Service) assetring.Ring { return s.Ring },
		Parse: func(fallback string, body []byte) ([]Service, error) {
			s, err := ParseService(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []Service{s}, nil
		},
		Save:      (*Catalog).SaveService,
		Check:     (*Catalog).checkService,
		FileFlag:  true,
		ShowShort: "Print a service unit",
		AddDoc: verbDoc{
			Use:   "add (-f FILE|- | <name> --exec … | <file>|-)",
			Short: "Add or replace a service unit in the local store",
			Long: "Add or replace a service unit in the local store, from a YAML document\n" +
				"(-f FILE, - = stdin) or from flags and --set paths. A unit named like a\n" +
				"built-in replaces it whole; use `set` for a sparse override instead.\n\n" +
				"An enabled unit whose port another enabled unit holds is refused.",
			Example: "  bashy service add -f ./openclaw.yaml\n" +
				"  bashy service add web --exec python3 --exec -m --exec http.server --exec '{port}' --port 8000",
		},
		SetDoc: verbDoc{
			Short: "Modify a service unit",
			Long:  "Modify a service unit. A built-in or shared unit gains a sparse local overlay.",
		},
		NameFlag: "store under this name instead of the document's own",
		AddCheck: func(s Service) error { return s.Validate() },
		Flags: []kindFlag{
			strFlag("description", "description", "what the unit serves", "what the unit serves", func(s *Service) *string { return &s.Description }),
			strFlag("provider", "provider", "process (default) | internal", "process | internal", func(s *Service) *string { return &s.Provider }),
			listFlag("exec", "exec", "argv element (repeatable, in order)", "argv element (repeatable; replaces exec)",
				func(s *Service, argv []string) error { s.Exec = argv; return nil }),
			recFlag("port", "ports.0.port", "primary listener port", "primary listener port", func(fs *pflag.FlagSet, p *int, n, h string) { fs.IntVar(p, n, 0, h) },
				func(s *Service, port int) error {
					if len(s.Ports) == 0 {
						s.Ports = []ServicePort{{}}
					}
					s.Ports[0].Port = port
					return nil
				}),
			strFlag("workdir", "workdir", "working directory", "working directory", func(s *Service) *string { return &s.Workdir }),
			listFlag("require", "requires", "a unit that must be active first (repeatable)", "a unit that must be active first (repeatable; replaces)",
				func(s *Service, r []string) error { s.Requires = r; return nil }),
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, s Service, _ bool) error {
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s, %s)\n", s.Name, s.ProviderName(), enabledWord(s.Enabled))
			return nil
		},
	})
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// newServiceToggle builds enable / disable: a desired-state write through the
// same overlay path `set` uses, so the port check and every other rule the
// record answers to apply unchanged. A running monitor reconciles the change
// live; nothing is started or stopped here.
func newServiceToggle(on bool, opts []Option) *cobra.Command {
	verb, short := "disable", "Persist a unit's desired state as disabled"
	if on {
		verb, short = "enable", "Persist a unit's desired state as enabled"
	}
	return &cobra.Command{
		Use:           verb + " <name>",
		Short:         short,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			before, ok := cat.Service(args[0])
			if !ok {
				return fmt.Errorf("fleet: no service %q", args[0])
			}
			if before.Enabled == on {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already %s\n", before.Name, enabledWord(on))
				return nil
			}
			if err := guardExisting(cmd.Context(), cat, KindService, before.Name, true); err != nil {
				return err
			}
			after := before
			after.Enabled = on
			if err := cat.checkService(after); err != nil {
				return err
			}
			if err := cat.saveChanged(dirServices, before.Name, before, after, []string{"enabled"}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", before.Name, enabledWord(on))
			return nil
		},
	}
}

type serviceRow struct {
	RecordLifecycle
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	Enabled     bool     `json:"enabled"`
	Ports       []string `json:"ports,omitempty"`
	Requires    []string `json:"requires,omitempty"`
	App         string   `json:"app,omitempty"`
	Mesh        []string `json:"mesh,omitempty"`
	Ring        string   `json:"ring"`
	Description string   `json:"description,omitempty"`
}

func newServiceList(opts []Option) *cobra.Command {
	var asJSON, retired bool
	c := &cobra.Command{
		Use:           "list",
		Short:         "List service units",
		Long:          serviceLong,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			units, errs := New(opts...).Services()
			rows := make([]serviceRow, 0, len(units))
			for _, s := range units {
				if !MatchRetirement(s, retired) {
					continue
				}
				row := serviceRow{
					RecordLifecycle: s.RecordLifecycle, Name: s.Name, Provider: s.ProviderName(), Enabled: s.Enabled,
					Requires: s.Dependencies(), Ring: s.Ring.String(), Description: s.Description,
				}
				for _, p := range s.Ports {
					row.Ports = append(row.Ports, bindLabel(p.Bind)+":"+strconv.Itoa(p.Port)+"/"+protoOf(p))
				}
				if a := s.Expose.App; a != nil {
					row.App = firstNonEmpty(a.Name, s.Name)
				}
				for _, m := range s.Expose.Mesh {
					row.Mesh = append(row.Mesh, m.Name)
				}
				rows = append(rows, row)
			}
			for _, e := range errs {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", e)
			}
			if asJSON {
				return WriteListJSON(cmd.OutOrStdout(), KindService, BasicView(retired, false), rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tPROVIDER\tSTATE\tPORTS\tEXPOSE\tRING")
			for _, r := range rows {
				expose := r.App
				if len(r.Mesh) > 0 {
					expose = strings.TrimPrefix(expose+",mesh:"+strings.Join(r.Mesh, "+"), ",")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.Provider, enabledWord(r.Enabled),
					dashIfEmpty(strings.Join(r.Ports, ",")), dashIfEmpty(expose), r.Ring)
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	c.Flags().BoolVar(&retired, "retired", false, "list retired units instead")
	return c
}
