package gomod

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// NewModCmd is `mod`: the Bash# module contract verbs. Every verb composes
// the go command or reads go.work/go.mod; none reimplements module logic.
func NewModCmd() *cobra.Command {
	var asJSON bool
	root := &cobra.Command{
		Use:   "mod",
		Short: "Bash# projects as Go modules: sibling pins are go.mod versions in a go.work workspace",
		Long: `mod treats every Bash# project as a Go module. A multi-repo workspace is a
go.work; a sibling pin is the go.mod require (or versioned fork replace).

  mod drift [DIR]          compare each sibling pin with the sibling's HEAD
  mod sync  [DIR]          go get every stale pin to the sibling's HEAD, then go mod tidy
  mod dir   PATH@VERSION   download a pinned module and print its directory
  mod tools [GOMOD]        list tool directives and the version that provides each
  mod init  [DIR]          make a project a Go module: go.mod + doc-only pkg.go
  mod proxy publish DIR    publish a module version into the local Go module proxy
  mod proxy status         list the local proxy's modules and versions

DIR defaults to the current directory; at the workspace root, drift and sync
cover every module. Exit codes: 3 a pin is stale, 5 a pin cannot be compared.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "emit a JSON envelope")
	mode := func() weavecli.OutputMode { return weavecli.ResolveOutputMode(asJSON, false, false) }
	ok := func(c *cobra.Command, name string, result any, text func(io.Writer)) error {
		if mode() == weavecli.OutputJSON {
			weavecli.EmitOK(c.OutOrStdout(), weavecli.OutputJSON, "mod "+name, result)
		} else {
			text(c.OutOrStdout())
		}
		return nil
	}
	fail := func(c *cobra.Command, name string, err error) error {
		weavecli.EmitError(c.ErrOrStderr(), mode(), "mod "+name, ExitCodeOf(err), err)
		return &Error{Code: ExitCodeOf(err), Msg: err.Error()}
	}
	argDir := func(args []string) string {
		if len(args) > 0 {
			return args[0]
		}
		return "."
	}
	// modules is the module at dir, or every module when dir is the
	// workspace root. A dir with no workspace yields nothing.
	modules := func(dir string) (*Workspace, []*Module, error) {
		ws, err := Load(dir)
		if err != nil || ws == nil {
			return nil, nil, err
		}
		if m := ws.Module(dir); m != nil {
			return ws, []*Module{m}, nil
		}
		if abs, err := filepath.Abs(dir); err == nil && filepath.Clean(abs) == ws.Root {
			return ws, ws.Modules, nil
		}
		return nil, nil, &Error{Code: weavecli.ExitInvalidArg, Msg: dir + " is not a module of " + ws.WorkFile}
	}

	root.AddCommand(&cobra.Command{
		Use:   "drift [DIR]",
		Short: "Compare each sibling pin with the sibling's HEAD",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ws, mods, err := modules(argDir(args))
			if err != nil {
				return fail(c, "drift", err)
			}
			var all []Drift
			for _, m := range mods {
				all = append(all, ws.Drift(m, nil)...)
			}
			code := weavecli.ExitOK
			for _, d := range all {
				if d.State == Stale {
					code = weavecli.ExitPrecondFail
				} else if d.State == Unknown && code == weavecli.ExitOK {
					code = weavecli.ExitDepUnhealthy
				}
			}
			_ = ok(c, "drift", map[string]any{"drift": all}, func(w io.Writer) {
				if ws == nil {
					fmt.Fprintln(w, "mod drift: no go.work; nothing to compare")
				}
				for _, d := range all {
					fmt.Fprintf(w, "%-8s %s -> %s %s %s\n", d.State, d.Module, d.Name, d.Version, d.Reason)
				}
			})
			if code != weavecli.ExitOK {
				return &Error{Code: code, Msg: "sibling pins are not in sync"}
			}
			return nil
		},
	})

	var only []string
	syncCmd := &cobra.Command{
		Use:   "sync [DIR]",
		Short: "go get every stale sibling pin to the sibling's HEAD, then go mod tidy",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ws, mods, err := modules(argDir(args))
			if err != nil {
				return fail(c, "sync", err)
			}
			var all []Change
			for _, m := range mods {
				ch, err := ws.Sync(context.Background(), m, only, nil, nil)
				all = append(all, ch...)
				if err != nil {
					return fail(c, "sync", err)
				}
			}
			return ok(c, "sync", map[string]any{"changes": all}, func(w io.Writer) {
				if len(all) == 0 {
					fmt.Fprintln(w, "mod sync: all pins in sync")
				}
				for _, ch := range all {
					fmt.Fprintf(w, "%s: %s %s -> %s\n", ch.Module, ch.Sibling, ch.From, ch.To)
				}
			})
		},
	}
	syncCmd.Flags().StringSliceVar(&only, "only", nil, "limit to these siblings (module path or name)")
	root.AddCommand(syncCmd)

	var offline bool
	dirCmd := &cobra.Command{
		Use:   "dir PATH@VERSION",
		Short: "Download a pinned module into the module cache and print its directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			info, err := ModuleDir(context.Background(), args[0], offline, nil)
			if err != nil {
				return fail(c, "dir", err)
			}
			return ok(c, "dir", info, func(w io.Writer) { fmt.Fprintln(w, info.Dir) })
		},
	}
	dirCmd.Flags().BoolVar(&offline, "offline", false, "answer from the module cache only (GOPROXY=off)")
	root.AddCommand(dirCmd)

	root.AddCommand(&cobra.Command{
		Use:   "tools [GOMOD]",
		Short: "List tool directives and the pinned module version providing each (- reads stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			path := "go.mod"
			if len(args) > 0 {
				path = args[0]
			}
			var data []byte
			var err error
			if path == "-" {
				data, err = io.ReadAll(c.InOrStdin())
			} else {
				data, err = os.ReadFile(path)
			}
			if err != nil {
				return fail(c, "tools", &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error()})
			}
			tools, err := Tools(data)
			if err != nil {
				return fail(c, "tools", &Error{Code: weavecli.ExitInvalidArg, Msg: err.Error()})
			}
			return ok(c, "tools", map[string]any{"tools": tools}, func(w io.Writer) {
				for _, t := range tools {
					fmt.Fprintf(w, "%s %s %s\n", t.Package, t.Module, t.Version)
				}
			})
		},
	})

	var modPath, goVer string
	initCmd := &cobra.Command{
		Use:   "init [DIR]",
		Short: "Make a project a Go module: go.mod (path from the origin remote) + doc-only pkg.go",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			res, err := Init(argDir(args), modPath, goVer)
			if err != nil {
				return fail(c, "init", err)
			}
			return ok(c, "init", res, func(w io.Writer) {
				for _, f := range res.Created {
					fmt.Fprintln(w, "created", f)
				}
				for _, f := range res.Unchanged {
					fmt.Fprintln(w, "unchanged", f)
				}
				for _, s := range res.Warnings {
					fmt.Fprintln(w, "warning:", s)
				}
			})
		},
	}
	initCmd.Flags().StringVar(&modPath, "module", "", "module path (default: derived from the origin remote)")
	initCmd.Flags().StringVar(&goVer, "go", "", "go directive version (default: the running toolchain's)")
	root.AddCommand(initCmd)

	proxyCmd := &cobra.Command{
		Use:   "proxy",
		Short: "The local Go module proxy: a GOPROXY-layout directory builds read with GOPROXY=file://DIR",
		Long: `proxy publishes module versions into a directory in Go module proxy layout
(<module>/@v/{list,V.info,V.mod,V.zip}). Builds read it with
GOPROXY=file://DIR; no server runs. DIR defaults to $` + ProxyEnv + `, else
$BASHY_HOME/gomod-proxy.`,
	}
	var proxyDir, version string
	publishCmd := &cobra.Command{
		Use:   "publish DIR",
		Short: "Publish the module at DIR (default version: the pseudo-version of HEAD); a published version is immutable",
		Long: `publish writes the module at DIR into the local proxy. Without --version
the version is the pseudo-version of the git commit at HEAD and the content is
that commit's tree under DIR. With --version the content is the working copy
of the files git tracks under DIR (every file outside a git repository).
Publishing identical content again is a no-op; different content under an
existing version is refused (exit 3): publish a new version.`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			dir := proxyDir
			if dir == "" {
				dir = ProxyDir()
			}
			pub, err := Publish(args[0], version, dir)
			if err != nil {
				return fail(c, "proxy publish", err)
			}
			return ok(c, "proxy publish", pub, func(w io.Writer) {
				state := "published"
				if !pub.Published {
					state = "unchanged"
				}
				fmt.Fprintf(w, "%s %s@%s %s\n", state, pub.Module, pub.Version, pub.Hash)
			})
		},
	}
	publishCmd.Flags().StringVar(&version, "version", "", "version to publish (default: the pseudo-version of HEAD)")
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "List the modules in the local proxy and their versions",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			dir := proxyDir
			if dir == "" {
				dir = ProxyDir()
			}
			mods, err := ProxyStatus(dir)
			if err != nil {
				return fail(c, "proxy status", err)
			}
			return ok(c, "proxy status", map[string]any{"proxy": dir, "modules": mods}, func(w io.Writer) {
				if len(mods) == 0 {
					fmt.Fprintln(w, "mod proxy: no modules in", dir)
				}
				for _, m := range mods {
					fmt.Fprintf(w, "%s %s\n", m.Module, strings.Join(m.Versions, " "))
				}
			})
		},
	}
	proxyCmd.PersistentFlags().StringVar(&proxyDir, "dir", "", "proxy directory (default $"+ProxyEnv+" or $BASHY_HOME/gomod-proxy)")
	proxyCmd.AddCommand(publishCmd, statusCmd)
	root.AddCommand(proxyCmd)
	return root
}

// Run executes `mod args...` and returns the weavecli exit code (the host's
// dispatch entry point).
func Run(args []string, stdout, stderr io.Writer) int {
	cmd := NewModCmd()
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.Execute()
	if _, ok := err.(*Error); err != nil && !ok {
		fmt.Fprintln(stderr, "mod:", err)
	}
	return ExitCodeOf(err)
}
