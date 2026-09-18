// Package zot runs Zot (a pure-Go, OCI-native image/artifact registry) as a
// managed external binary (pkg/binmgr) — the OCI registry for the dhnt mesh.
// The Zot binary is downloaded → sha256-verified → cached by binmgr, never
// compiled in; bashy ("the OS of binaries") launches it via `bashy zot`, and
// outpost exposes it over the mesh as `registry`. project-zot/zot is Apache-2.0.
// See dhnt/docs/external-binary-builtins.md + docs/mesh-wrap-stack.md.
package zot

import (
	"context"
	"encoding/json"
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
	// DefaultVersion pins the Zot release. "" / "latest" resolves the newest
	// release; pin for reproducibility ($ZOT_VERSION or --zot-version override).
	DefaultVersion = "latest"
	DefaultAddr    = "127.0.0.1"
	DefaultPort    = 5000
)

// Spec is the binmgr GitHub spec zot resolves its binary from. zot ships several
// per-platform raw binaries (full, minimal, exporter) — match the full build.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name: "zot", Repo: "project-zot/zot", Version: version,
		AssetMatch: assetMatch,
	}
}

// assetMatch picks the full `zot-<os>-<arch>` binary by EXACT name.
//
// Substring matching is wrong here and shipped a real bug: a zot release
// publishes several per-platform binaries that ALL carry the os/arch tokens —
//
//	zb-darwin-arm64           the load-test BENCHMARK tool
//	zli-darwin-arm64          the CLI
//	zot-darwin-arm64          the registry server (what we want)
//	zot-darwin-arm64-debug    debug build
//	zot-darwin-arm64-minimal  reduced build
//	zxp-darwin-arm64          the metrics exporter
//
// The previous predicate excluded only "minimal" and "exporter", so FOUR of
// those still matched — and the resolver cached `zb`, which runs benchmarks
// instead of serving. `bashy zot serve` printed "Preparing test data ... /
// Starting tests ..." and nothing ever listened on the registry port. Note the
// "exporter" exclusion was also dead code: the asset is named `zxp`.
//
// An exact match is both correct and immune to new sibling binaries appearing
// in a future release.
func assetMatch(name, goos, goarch string) bool {
	return strings.EqualFold(name, "zot-"+goos+"-"+goarch)
}

// DefaultDataDir is zot's storage root (the registry blob store + config).
func DefaultDataDir() string {
	if d := os.Getenv("ZOT_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "zot")
	}
	return filepath.Join(home, ".agents", "bashy", "zot")
}

// Options configures Serve/Start.
type Options struct {
	Version string
	DataDir string
	Addr    string
	Port    int
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

// Instance is a running zot registry.
type Instance struct {
	URL     string
	Addr    string
	Version string
	proc    *binmgr.Process
}

// Stop terminates the zot process gracefully.
func (i *Instance) Stop() error {
	if i == nil || i.proc == nil {
		return nil
	}
	return i.proc.Stop(10 * time.Second)
}

// Start resolves + launches `zot serve <config>` bound to a loopback port
// (NON-blocking): it returns once the registry answers. The config is seeded on
// first run (storage root + loopback bind). The caller owns the Instance — this
// is what outpost's wrap-harness builtin supervises and exposes over the mesh.
func Start(ctx context.Context, o Options) (*Instance, error) {
	o.defaults()
	tool, err := binmgr.ResolveGitHub(ctx, Spec(o.Version))
	if err != nil {
		return nil, fmt.Errorf("zot: resolve: %w", err)
	}
	bin, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return nil, fmt.Errorf("zot: fetch: %w", err)
	}
	cfg, err := ensureConfig(o.DataDir, o.Addr, o.Port)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", o.Addr, o.Port)
	url := "http://" + addr
	proc, err := binmgr.Launch(ctx, bin, binmgr.RunSpec{
		Args:       []string{"serve", cfg},
		Dir:        o.DataDir,
		Stdout:     o.Stdout,
		Stderr:     o.Stderr,
		HealthURL:  url + "/v2/", // the OCI distribution API root
		HealthWait: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("zot: start: %w", err)
	}
	return &Instance{URL: url, Addr: addr, Version: tool.Version, proc: proc}, nil
}

// Serve runs zot in the foreground, blocking until ctx is cancelled or a signal
// arrives (the `bashy zot serve` path).
func Serve(ctx context.Context, o Options) error {
	o.defaults()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	inst, err := Start(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Stdout, "zot (%s) serving on %s — storage %s\n", inst.Version, inst.URL, o.DataDir)
	fmt.Fprintln(o.Stdout, "expose it over the mesh:  outpost mesh service add registry "+inst.Addr)

	<-ctx.Done()
	fmt.Fprintln(o.Stdout, "zot: shutting down…")
	return inst.Stop()
}

// ensureConfig writes a minimal zot config.json on first run, returns its path.
func ensureConfig(dataDir, addr string, port int) (string, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dataDir, "config.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return cfgPath, nil // reuse
	}
	cfg := map[string]any{
		"storage": map[string]any{"rootDirectory": filepath.Join(dataDir, "registry")},
		"http":    map[string]any{"address": addr, "port": strconv.Itoa(port)},
		"log":     map[string]any{"level": "info"},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		return "", err
	}
	return cfgPath, nil
}

// NewZotCmd builds the `zot` command tree (bashy front-door + any cobra host).
func NewZotCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "zot",
		Short: "Run Zot (the mesh OCI registry) as a managed external binary",
		Long: `zot runs the Zot OCI registry — downloaded, sha256-verified, and cached by
binmgr (not compiled in). Expose it over the mesh with:
outpost mesh service add registry <addr>. It also serves Ollama models (OCI).`,
	}
	var o Options
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Download (if needed) + run the zot registry on a loopback port",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Serve(cmd.Context(), o)
		},
	}
	serve.Flags().StringVar(&o.Version, "zot-version", "", "Zot release tag (default: latest)")
	serve.Flags().StringVar(&o.DataDir, "data", "", "storage dir (default ~/.agents/bashy/zot)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "HTTP bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "HTTP port")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached zot binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := binmgr.ResolveGitHub(cmd.Context(), Spec(o.Version))
			if err != nil {
				return err
			}
			p, err := binmgr.Ensure(cmd.Context(), tool)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t(zot %s)\n", p, tool.Version)
			return nil
		},
	}
	pathCmd.Flags().StringVar(&o.Version, "zot-version", "", "Zot release tag (default: latest)")

	root.AddCommand(serve, pathCmd)
	return root
}
