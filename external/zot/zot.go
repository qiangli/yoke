// Package zot runs Zot (a pure-Go, OCI-native image/artifact registry) as a
// managed external binary (pkg/binmgr) — the OCI registry for the dhnt mesh.
// The Zot binary is downloaded → sha256-verified → cached by binmgr, never
// compiled in; bashy ("the OS of binaries") launches it via `bashy zot`, and
// outpost exposes it over the mesh as `registry`. Upstream project-zot/zot is
// Apache-2.0; the bashy fork qiangli/zot is the signed supply-chain pin (minimal
// build, BinaryType bashy-minimal: no search/sync/imagetrust/ui extensions).
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

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// Upstream release that the bashy fork was built from (builds/zot at the
	// pinned commit). The fork tag is the upstream tag plus the bashy build counter.
	zotVersion = "v2.1.22"
	// Bashy-signed fork release tag (builds/zot/tool.env at the pin):
	// built by the bashy-build workflow on qiangli/zot and published as
	// raw executables with per-asset SHA256SUMS and <asset>.sigstore.json bundles.
	// This is a MINIMAL build (no search/sync/imagetrust/ui extensions;
	// BinaryType bashy-minimal).
	zotBashyTag = "bashy-v2.1.22-3"

	// DefaultVersion is the pinned bashy tag. There is no "latest" — that was
	// the upstream archive and it is intentionally absent (fail closed).
	DefaultVersion = zotBashyTag
	DefaultAddr    = "127.0.0.1"
	DefaultPort    = 5000
)

// SHA256 per asset read from the published SHA256SUMS of the bashy release
// (downloaded via curl -sL, not from the GitHub API asset digest).
var zotSHAs = map[string]string{
	"linux/amd64":   "3586360d1ff6518ccd699d7d7bae81bbad298017183a32b80a63366a016a8800",
	"linux/arm64":   "9cc8e193e4e91ecc1ca9f322443bab7240203ac45734a47e3e8324b3bf218499",
	"darwin/amd64":  "2bfc95f6989ba65017e887ad74158d679d5dc1449c95aa654dd42b560efab2b2",
	"darwin/arm64":  "3b9b24f2f848de8d8da0783d46b5a16ec76045ecbcc036b25645f232b764d216",
	"windows/amd64": "c373f8f85f5324624d6bb11620fe149fec5c2131bcf443234f5a2f10780e294c",
}

func zotTool() binmgr.Tool {
	assets := map[string]binmgr.Asset{}
	for plat, sha := range zotSHAs {
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := "zot-" + goos + "-" + goarch
		if goos == "windows" {
			assetName += ".exe"
		}
		url := fmt.Sprintf("https://github.com/qiangli/zot/releases/download/%s/%s", zotBashyTag, assetName)
		assets[plat] = binmgr.Asset{URL: url, SHA256: sha}
	}
	identity := fmt.Sprintf("https://github.com/qiangli/zot/.github/workflows/bashy-build.yml@refs/tags/%s", zotBashyTag)
	return binmgr.Tool{
		Name:    "zot",
		Version: zotBashyTag,
		Assets:  assets,
		Sigstore: &binmgr.Sigstore{
			Identity: identity,
			Issuer:   binmgr.GitHubActionsIssuer,
		},
	}
}

// ZotTool is the pinned, Sigstore-verified tool declaration. Exported
// for tests and for callers that need to inspect the pin without launching.
var ZotTool = zotTool()

// Spec is retained for compatibility with callers that probe the resolver's
// asset matcher (e.g. pkg/atlas/platform_external_test). It returns a
// GitHubSpec for the qiangli fork, not the upstream project-zot/zot
// archive, and never returns "latest".
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	if version == zotVersion {
		version = zotBashyTag
	}
	return binmgr.GitHubSpec{
		Name: "zot", Repo: "qiangli/zot", Version: version,
		AssetMatch: assetMatch,
	}
}

func resolveTool(version string) (binmgr.Tool, error) {
	tool := ZotTool
	switch version {
	case "", zotBashyTag, zotVersion:
		return tool, nil
	default:
		return binmgr.Tool{}, fmt.Errorf("zot: version %q not supported: only %q (bashy-signed) is available (upstream %q alias accepted)", version, zotBashyTag, zotVersion)
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
	expected := "zot-" + goos + "-" + goarch
	if goos == "windows" {
		expected += ".exe"
	}
	return strings.EqualFold(name, expected)
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
	tool, err := resolveTool(o.Version)
	if err != nil {
		return nil, err
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
		Env:        serviceenv.LocalOTLP(nil, "zot"),
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
		// Zot has no update checker or anonymous usage reporter. Keeping the
		// optional extensions map empty also leaves sync, CVE feeds, events and
		// remote metrics disabled. Configuration reference:
		// https://github.com/project-zot/zot/blob/main/examples/README.md
		"extensions": map[string]any{},
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
	serve.Flags().StringVar(&o.Version, "zot-version", "", "Zot release tag (default: "+DefaultVersion+")")
	serve.Flags().StringVar(&o.DataDir, "data", "", "storage dir (default ~/.agents/bashy/zot)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "HTTP bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "HTTP port")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached zot binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := resolveTool(o.Version)
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
	pathCmd.Flags().StringVar(&o.Version, "zot-version", "", "Zot release tag (default: "+DefaultVersion+")")

	root.AddCommand(serve, pathCmd)
	return root
}
