package gomod

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/artifact"
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
  mod proxy serve          serve the embedded Go module proxy over CAS + Loom

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
		return &Error{Code: ExitCodeOf(err), Msg: err.Error(), Err: err}
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
		Short: "The local Go module proxy: directory layout or embedded CAS + Loom HTTP server",
		Long: `proxy publishes module versions into a GOPROXY-layout directory
(GOPROXY=file://DIR) or, with --catalog / --artifact-root, into the protected
CAS artifact catalog backed by durable Loom Git refs served over loopback HTTP
by 'mod proxy serve'.`,
	}
	var proxyDir, version string
	var pubCatalog bool
	var pubArtifactRoot, pubLoomDir string
	publishCmd := &cobra.Command{
		Use:   "publish DIR",
		Short: "Publish the module at DIR (default version: the pseudo-version of HEAD); a published version is immutable",
		Long: `publish writes the module at DIR into the local proxy. Without --version
the version is the pseudo-version of the git commit at HEAD and the content is
that commit's tree under DIR. With --catalog or --artifact-root, the exact git
commit and immutable Loom ref are persisted first and canonical .info/.mod/.zip
artifacts are recorded in the CAS catalog. Publishing identical content again is
a no-op; different content under an existing version is refused (exit 3).`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if pubCatalog || strings.TrimSpace(pubArtifactRoot) != "" {
				pub, err := PublishArtifact(c.Context(), PublishOptions{
					Dir:          args[0],
					Version:      version,
					ArtifactRoot: pubArtifactRoot,
					LoomRepoDir:  pubLoomDir,
				})
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
			}
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
	publishCmd.Flags().BoolVar(&pubCatalog, "catalog", false, "publish into the protected CAS catalog + durable Loom repository")
	publishCmd.Flags().StringVar(&pubArtifactRoot, "artifact-root", "", "artifact store root directory (implies --catalog)")
	publishCmd.Flags().StringVar(&pubLoomDir, "loom-dir", "", "durable bare Loom Git repository directory (default <artifact-root>/loom/modules.git)")

	var statusCatalog bool
	var statusArtifactRoot string
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "List the modules in the local proxy and their versions",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if statusCatalog || strings.TrimSpace(statusArtifactRoot) != "" {
				artRoot := strings.TrimSpace(statusArtifactRoot)
				if artRoot == "" {
					artRoot = artifact.DefaultRoot()
				}
				mods, err := CatalogProxyStatus(c.Context(), CatalogStatusOptions{ArtifactRoot: artRoot})
				if err != nil {
					return fail(c, "proxy status", err)
				}
				if mods == nil {
					mods = []CatalogModuleStatus{}
				}
				return ok(c, "proxy status", map[string]any{"artifact_root": artRoot, "modules": mods}, func(w io.Writer) {
					if len(mods) == 0 {
						fmt.Fprintln(w, "mod proxy: no modules in", artRoot)
						return
					}
					for _, m := range mods {
						for _, p := range m.Publications {
							zipState := "present"
							if !p.ZipPresent {
								zipState = "evicted"
							}
							fmt.Fprintf(w, "%s %s %s zip=%s\n", m.Module, p.Version, p.Hash, zipState)
						}
					}
				})
			}
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
	statusCmd.Flags().BoolVar(&statusCatalog, "catalog", false, "read published modules from the protected CAS catalog")
	statusCmd.Flags().StringVar(&statusArtifactRoot, "artifact-root", "", "artifact store root directory (implies --catalog)")

	var serveAddr, serveArtifactRoot, serveLoomDir string
	var serveSealed, serveAllowMesh bool
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the embedded Go module proxy over the CAS catalog and Loom repository",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, args []string) error {
			baseCtx := c.Context()
			if baseCtx == nil {
				baseCtx = context.Background()
			}
			sigCtx, stop := signal.NotifyContext(baseCtx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			srv, err := StartProxyServer(sigCtx, ProxyServerOptions{
				Addr:          serveAddr,
				AllowMeshBind: serveAllowMesh,
				ArtifactRoot:  serveArtifactRoot,
				LoomRepoDir:   serveLoomDir,
				Sealed:        serveSealed,
			})
			if err != nil {
				return fail(c, "proxy serve", err)
			}
			defer srv.Close()

			if mode() == weavecli.OutputJSON {
				weavecli.EmitOK(c.OutOrStdout(), weavecli.OutputJSON, "mod proxy serve", map[string]any{
					"url":    srv.URL(),
					"addr":   srv.Addr(),
					"sealed": srv.Sealed(),
				})
			} else {
				fmt.Fprintln(c.OutOrStdout(), srv.URL())
			}

			<-sigCtx.Done()
			return srv.Close()
		},
	}
	serveCmd.Flags().StringVar(&serveAddr, "addr", "127.0.0.1:0", "bind address for the HTTP Go module proxy")
	serveCmd.Flags().StringVar(&serveArtifactRoot, "artifact-root", "", "artifact store root directory")
	serveCmd.Flags().StringVar(&serveLoomDir, "loom-dir", "", "durable bare Loom Git repository directory")
	serveCmd.Flags().BoolVar(&serveSealed, "sealed", false, "disable all upstream/public network fetching")
	serveCmd.Flags().BoolVar(&serveAllowMesh, "allow-mesh-bind", false, "permit binding non-loopback addresses")

	proxyCmd.PersistentFlags().StringVar(&proxyDir, "dir", "", "proxy directory (default $"+ProxyEnv+" or $BASHY_HOME/gomod-proxy)")
	proxyCmd.AddCommand(publishCmd, statusCmd, serveCmd)
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
