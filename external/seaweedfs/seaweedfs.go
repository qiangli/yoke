// Package seaweedfs runs SeaweedFS (a pure-Go object/blob store with an S3
// gateway) as a managed external binary (pkg/binmgr) — the storage spine for the
// dhnt mesh (it can also back the Zot registry's blob store). The `weed` binary
// is downloaded → sha256-verified → cached by binmgr, never compiled in; bashy
// launches it via `bashy seaweedfs`, and outpost exposes its S3 gateway over the
// mesh as `s3`. seaweedfs/seaweedfs is Apache-2.0.
// See dhnt/docs/external-binary-builtins.md + docs/mesh-wrap-stack.md.
package seaweedfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// DefaultVersion pins the SeaweedFS release. "" / "latest" resolves the
	// newest; pin for reproducibility ($SEAWEEDFS_VERSION or --version override).
	DefaultVersion = "latest"
	DefaultAddr    = "127.0.0.1"
	DefaultPort    = 8333 // the S3 gateway port
)

// Spec is the binmgr GitHub spec. SeaweedFS ships per-platform .tar.gz archives
// named `<os>_<arch>.tar.gz` containing a single `weed` binary; match the
// standard build (not the _full / _large_disk variants).
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name: "seaweedfs", Repo: "seaweedfs/seaweedfs", Version: version,
		Member: "weed", AssetMatch: assetMatch,
	}
}

// assetMatch picks the standard `<os>_<arch>.tar.gz`, excluding the _full and
// _large_disk variants that also carry the os/arch tokens.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if strings.Contains(n, "full") || strings.Contains(n, "large_disk") {
		return false
	}
	return strings.Contains(n, goos) && strings.Contains(n, goarch)
}

// DefaultDataDir is SeaweedFS's storage root.
func DefaultDataDir() string {
	if d := os.Getenv("SEAWEEDFS_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "seaweedfs")
	}
	return filepath.Join(home, ".agents", "bashy", "seaweedfs")
}

// Options configures Serve/Start.
type Options struct {
	Version string
	DataDir string
	Addr    string
	Port    int // the S3 gateway port
	Stdout  io.Writer
	Stderr  io.Writer
}

func (o *Options) defaults() {
	if o.Version == "" {
		o.Version = DefaultVersion
	}
	if o.DataDir == "" {
		o.DataDir = DefaultDataDir()
	}
	if o.Addr == "" {
		o.Addr = DefaultAddr
	}
	if o.Port == 0 {
		o.Port = DefaultPort
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
}

// Instance is a running SeaweedFS server.
type Instance struct {
	URL     string // the S3 gateway URL
	Addr    string // addr:port of the S3 gateway
	Version string
	proc    *binmgr.Process
}

// Stop terminates the weed process gracefully.
func (i *Instance) Stop() error {
	if i == nil || i.proc == nil {
		return nil
	}
	return i.proc.Stop(10 * time.Second)
}

// Start resolves + launches `weed server` (master+volume+filer+S3, all bound to
// loopback) NON-blocking, returning once the S3 gateway answers. The caller owns
// the Instance — this is what outpost's wrap-harness builtin supervises and
// exposes over the mesh. SeaweedFS runs from flags; no config file is seeded.
func Start(ctx context.Context, o Options) (*Instance, error) {
	o.defaults()
	tool, err := binmgr.ResolveGitHub(ctx, Spec(o.Version))
	if err != nil {
		return nil, fmt.Errorf("seaweedfs: resolve: %w", err)
	}
	bin, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return nil, fmt.Errorf("seaweedfs: fetch: %w", err)
	}
	if err := os.MkdirAll(o.DataDir, 0o755); err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", o.Addr, o.Port)
	url := "http://" + addr
	proc, err := binmgr.Launch(ctx, bin, binmgr.RunSpec{
		Args: []string{
			"server",
			"-dir=" + o.DataDir,
			"-ip.bind=" + o.Addr, // bind every service to loopback
			"-s3",
			"-s3.port=" + strconv.Itoa(o.Port),
		},
		Dir:        o.DataDir,
		Stdout:     o.Stdout,
		Stderr:     o.Stderr,
		HealthURL:  url + "/", // the S3 gateway answers once the stack is up
		HealthWait: 45 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("seaweedfs: start: %w", err)
	}
	return &Instance{URL: url, Addr: addr, Version: tool.Version, proc: proc}, nil
}

// Serve runs SeaweedFS in the foreground, blocking until ctx is cancelled or a
// signal arrives (the `bashy seaweedfs serve` path).
func Serve(ctx context.Context, o Options) error {
	o.defaults()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	inst, err := Start(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Stdout, "seaweedfs (%s) S3 gateway on %s — storage %s\n", inst.Version, inst.URL, o.DataDir)
	fmt.Fprintln(o.Stdout, "expose it over the mesh:  outpost mesh service add s3 "+inst.Addr)

	<-ctx.Done()
	fmt.Fprintln(o.Stdout, "seaweedfs: shutting down…")
	return inst.Stop()
}

// NewSeaweedfsCmd builds the `seaweedfs` command tree (bashy front-door).
func NewSeaweedfsCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "seaweedfs",
		Short: "Run SeaweedFS (the mesh object/blob store, S3 gateway) as a managed external binary",
		Long: `seaweedfs runs SeaweedFS — downloaded, sha256-verified, and cached by binmgr
(not compiled in). Exposes an S3 gateway; expose it over the mesh with:
outpost mesh service add s3 <addr>. Can also back the Zot registry's blob store.`,
	}
	var o Options
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Download (if needed) + run SeaweedFS (S3 gateway) on a loopback port",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Serve(cmd.Context(), o)
		},
	}
	serve.Flags().StringVar(&o.Version, "version", "", "SeaweedFS release tag (default: latest)")
	serve.Flags().StringVar(&o.DataDir, "data", "", "storage dir (default ~/.agents/bashy/seaweedfs)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "S3 gateway port")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached weed binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := binmgr.ResolveGitHub(cmd.Context(), Spec(o.Version))
			if err != nil {
				return err
			}
			p, err := binmgr.Ensure(cmd.Context(), tool)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t(seaweedfs %s)\n", p, tool.Version)
			return nil
		},
	}
	pathCmd.Flags().StringVar(&o.Version, "version", "", "SeaweedFS release tag (default: latest)")

	root.AddCommand(serve, pathCmd)
	return root
}
