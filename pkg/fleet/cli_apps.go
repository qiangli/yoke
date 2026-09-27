package fleet

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewAppCmds builds the registered-app CRUD words the Apps console mounts
// under its own `app` verb: show · schema · add · set · rm · edit. `list` is
// the console's (it merges every panel source and probes liveness).
func NewAppCmds(opts ...Option) []*cobra.Command {
	return []*cobra.Command{
		newAppsShow(opts),
		newSchema(KindApp),
		newAppsAdd(opts),
		newAppsSet(opts),
		newRm(KindApp, opts, (*Catalog).RemoveApp),
		newEdit(KindApp, opts, (*Catalog).MaterializeApp),
	}
}

// AppsHint is printed after a write: the console mounts panels when it
// starts, so a running one picks the change up on restart.
const AppsHint = "takes effect when the console (re)starts: bashy app service stop && bashy app service start"

func newAppsShow(opts []Option) *cobra.Command {
	var asJSON, asYAML bool
	var field string
	c := &cobra.Command{
		Use:           "show <name>",
		Short:         "Print a registered app's record",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(asJSON, asYAML); err != nil {
				return err
			}
			a, ok := New(opts...).App(args[0])
			if !ok {
				return fmt.Errorf("fleet: no registered app %q", args[0])
			}
			if field != "" {
				if err := emitField(cmd.OutOrStdout(), a, KindApp, field, asJSON); err != nil {
					return reportPathError(cmd, KindApp, err)
				}
				return nil
			}
			return emit(cmd.OutOrStdout(), a, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of the canonical YAML")
	c.Flags().BoolVar(&asYAML, "yaml", false, "emit the canonical YAML record (the default)")
	c.Flags().StringVar(&field, "field", "", "print one dotted path")
	return c
}

// appFlags are the named fields `add` and `set` take directly; anything else
// goes through --set path=value.
type appFlags struct {
	port                          int
	label, icon, tip, auth, login string
	start                         []string
	paths                         pathFlags
}

func (f *appFlags) bind(c *cobra.Command) {
	c.Flags().IntVar(&f.port, "port", 0, "loopback port the app listens on")
	c.Flags().StringVar(&f.label, "label", "", "tile label (default: the name)")
	c.Flags().StringVar(&f.icon, "icon", "", "one emoji, or SVG path data on a 24 grid")
	c.Flags().StringVar(&f.tip, "tip", "", "tile tooltip")
	c.Flags().StringArrayVar(&f.start, "start", nil, "start-hint argv element shown when the app is down (repeatable; never run)")
	c.Flags().StringVar(&f.auth, "auth", "", "auth tier: system (default), public or custom")
	c.Flags().StringVar(&f.login, "login-path", "", "custom auth only: app-relative login path")
	f.paths.bind(c)
}

func (f *appFlags) apply(cmd *cobra.Command, a *App) error {
	ch := cmd.Flags().Changed
	if ch("port") {
		a.Port = f.port
	}
	if ch("label") {
		a.Label = f.label
	}
	if ch("icon") {
		a.Icon = f.icon
	}
	if ch("tip") {
		a.Tip = f.tip
	}
	if ch("start") {
		a.Start = f.start
	}
	if ch("auth") {
		a.Auth = f.auth
	}
	if ch("login-path") {
		a.LoginPath = f.login
	}
	return applyPathFlags(cmd, KindApp, a, f.paths)
}

func newAppsAdd(opts []Option) *cobra.Command {
	var f appFlags
	c := &cobra.Command{
		Use:     "add (<name> --port N [flags] | <file>|-)",
		Aliases: []string{"register"},
		Short:   "Register a local web app as a console tile",
		Long: "Register a local web server as an Apps tile, proxied at /<name>/ to\n" +
			"127.0.0.1:<port>. The record is read as data: the console never runs\n" +
			"anything to discover a registered app. A name the console already uses\n" +
			"(a builtin panel, an atlas surface, a reserved mount) is refused.\n\n" +
			AppsHint + ".",
		Example: "  bashy app add jupyter --port 8888 --label Jupyter --icon 📓 --start jupyter --start lab\n" +
			"  bashy app add ./grafana.yaml",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			var a App
			if looksLikePath(args[0]) && cmd.Flags().NFlag() == 0 {
				data, err := readSource(args[0], cmd.InOrStdin())
				if err != nil {
					return err
				}
				if a, err = ParseApp(baseName(args[0]), data, nil); err != nil {
					return err
				}
			} else {
				a = App{Name: args[0], Kind: KindApp}
				if err := f.apply(cmd, &a); err != nil {
					return err
				}
			}
			if err := cat.claimName(KindApp, a.Name, nil, false); err != nil {
				return err
			}
			if err := cat.SaveApp(a); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s -> /%s/ (127.0.0.1:%d)\n", a.Name, a.Name, a.Port)
			fmt.Fprintln(cmd.ErrOrStderr(), "note: "+AppsHint)
			return nil
		},
	}
	f.bind(c)
	return c
}

func newAppsSet(opts []Option) *cobra.Command {
	var f appFlags
	c := &cobra.Command{
		Use:           "set <name>",
		Short:         "Modify a registered app",
		Long:          "Modify a registered app. An entry from a shared ring is copied into the local store first.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			a, ok := cat.App(args[0])
			if !ok {
				return fmt.Errorf("fleet: no registered app %q", args[0])
			}
			from := a.Ring
			if err := f.apply(cmd, &a); err != nil {
				return err
			}
			if err := cat.claimName(KindApp, a.Name, nil, false); err != nil {
				return err
			}
			if err := cat.SaveApp(a); err != nil {
				return err
			}
			if from != ringLocal() {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: copied %s from the %s ring into the local store\n", a.Name, from)
			}
			fmt.Fprintln(cmd.OutOrStdout(), a.Name)
			fmt.Fprintln(cmd.ErrOrStderr(), "note: "+AppsHint)
			return nil
		},
	}
	f.bind(c)
	return c
}
