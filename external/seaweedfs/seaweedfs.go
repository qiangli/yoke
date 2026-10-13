// Package seaweedfs runs SeaweedFS (a pure-Go object/blob store with an S3
// gateway) as a managed external binary (pkg/binmgr) — the storage spine for the
// dhnt mesh (it can also back the Zot registry's blob store). The `weed` binary
// is downloaded → sha256-verified → cached by binmgr, never compiled in; bashy
// launches it via `bashy seaweedfs`, and outpost exposes its S3 gateway over the
// mesh as `s3`. Upstream seaweedfs/seaweedfs is Apache-2.0; the bashy fork
// qiangli/seaweedfs is the signed supply-chain pin.
// See dhnt/docs/external-binary-builtins.md + docs/mesh-wrap-stack.md.
package seaweedfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// Upstream release that the bashy fork was built from (builds/seaweedfs
	// at the pinned commit). The fork tag is the upstream tag plus the bashy
	// build counter.
	seaweedfsVersion = "4.48"
	// Bashy-signed fork release tag (builds/seaweedfs/tool.env at the pin):
	// built by the bashy-build workflow on qiangli/seaweedfs and published as
	// raw executables with per-asset SHA256SUMS and <asset>.sigstore.json bundles.
	// Workflow run 38330302167 green: go1.26.9 + golang.org/x/net v0.60.0, 0 reachable.
	seaweedfsBashyTag = "bashy-4.48-1"

	// DefaultVersion is the pinned bashy tag. There is no "latest" — that was
	// the upstream archive and it is intentionally absent (fail closed).
	DefaultVersion = seaweedfsBashyTag
	DefaultAddr    = "127.0.0.1"
	DefaultPort    = 8333 // the S3 gateway port
)

// SHA256 per asset read from the published SHA256SUMS of the bashy release
// (downloaded via curl -sL, not from the GitHub API asset digest).
var seaweedfsSHAs = map[string]string{
	"linux/amd64":  "7b6364ccea4f8cc2940e7a5da93f4e6a9dcd8629a2ec69bfe8e59e6541bd2889",
	"linux/arm64":  "44afeacee5bda0d71a4a2cad1530657cb442873a37bf6a91fe4d8828e445b431",
	"darwin/amd64": "4a6b8d405cc82ed95edac9f9d96f125408eb6f436e30d203a92493eaf97aa2d0",
	"darwin/arm64": "925de1501ef5a297d2a26efafc096e56ce537c67fb71f5a7899df510cf6ad2dc",
}

func seaweedfsTool() binmgr.Tool {
	assets := map[string]binmgr.Asset{}
	for plat, sha := range seaweedfsSHAs {
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := "weed-" + goos + "-" + goarch
		url := fmt.Sprintf("https://github.com/qiangli/seaweedfs/releases/download/%s/%s", seaweedfsBashyTag, assetName)
		assets[plat] = binmgr.Asset{URL: url, SHA256: sha}
	}
	identity := fmt.Sprintf("https://github.com/qiangli/seaweedfs/.github/workflows/bashy-build.yml@refs/tags/%s", seaweedfsBashyTag)
	return binmgr.Tool{
		Name:    "seaweedfs",
		Version: seaweedfsBashyTag,
		Assets:  assets,
		Sigstore: &binmgr.Sigstore{
			Identity: identity,
			Issuer:   binmgr.GitHubActionsIssuer,
		},
	}
}

// SeaweedfsTool is the pinned, Sigstore-verified tool declaration. Exported
// for tests and for callers that need to inspect the pin without launching.
var SeaweedfsTool = seaweedfsTool()

// Spec is retained for compatibility with callers that probe the resolver's
// asset matcher (e.g. pkg/atlas/platform_external_test). It returns a
// GitHubSpec for the qiangli fork, not the upstream seaweedfs/seaweedfs
// archive, and never returns "latest".
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	// Normalize the upstream tag to the bashy tag so "4.48" still resolves
	// to the signed fork without opening an upstream path.
	if version == seaweedfsVersion {
		version = seaweedfsBashyTag
	}
	return binmgr.GitHubSpec{
		Name: "seaweedfs", Repo: "qiangli/seaweedfs", Version: version,
		Member: "", AssetMatch: assetMatch,
	}
}

// assetMatch picks the raw `weed-<goos>-<goarch>` executable published by the
// bashy fork. The upstream archive was `<os>_<arch>.tar.gz` with _full and
// _large_disk variants; the fork publishes only raw executables.
func assetMatch(name, goos, goarch string) bool {
	return strings.EqualFold(name, "weed-"+goos+"-"+goarch)
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

func resolveTool(version string) (binmgr.Tool, error) {
	if runtime.GOOS == "windows" {
		return binmgr.Tool{}, fmt.Errorf("seaweedfs: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed")
	}
	tool := SeaweedfsTool
	// Accept empty, the bashy tag, or the upstream version as aliases for the pinned tool.
	// "latest" and any other version are refused — there is no upstream archive fallback.
	switch version {
	case "", seaweedfsBashyTag, seaweedfsVersion:
		return tool, nil
	default:
		return binmgr.Tool{}, fmt.Errorf("seaweedfs: version %q not supported: only %q (bashy-signed) is available (upstream %q alias accepted)", version, seaweedfsBashyTag, seaweedfsVersion)
	}
}

// Start resolves + launches `weed server` (master+volume+filer+S3, all bound to
// loopback) NON-blocking, returning once the S3 gateway answers. The caller owns
// the Instance — this is what outpost's wrap-harness builtin supervises and
// exposes over the mesh. SeaweedFS runs from flags; no config file is seeded.
func Start(ctx context.Context, o Options) (*Instance, error) {
	o.defaults()
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("seaweedfs: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed")
	}
	tool, err := resolveTool(o.Version)
	if err != nil {
		return nil, err
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
		Args:       serverArgs(o),
		Env:        serviceenv.LocalOTLP(nil, "seaweedfs"),
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

func serverArgs(o Options) []string {
	return []string{
		"server",
		"-dir=" + o.DataDir,
		// Loopback binding for every service. Upstream flags (4.48):
		//   server.go:59  serverIp = cmdServer.Flag.String("ip", ..., "ip or server name, also used as identifier")
		//   server.go:60  serverBindIp = cmdServer.Flag.String("ip.bind", "", "ip address to bind to. If empty, default to same as -ip option.")
		// Setting both -ip and -ip.bind to loopback guarantees the master/volume/filer/S3 listeners
		// bind only to 127.0.0.1 regardless of viper or detected host address.
		"-ip=" + o.Addr,
		"-ip.bind=" + o.Addr,
		"-s3",
		"-s3.port=" + strconv.Itoa(o.Port),
		// SeaweedFS otherwise sends anonymous cluster statistics to
		// telemetry.seaweedfs.com once 1 GiB is stored. The flag defaults to ON:
		//   server.go:113  masterOptions.telemetryEnabled = cmdServer.Flag.Bool("master.telemetry", true, "report anonymous cluster statistics to master.telemetry.url, use -master.telemetry=false to opt out")
		// The same surface exists on other subcommands that start a master:
		//   master.go:115  m.telemetryEnabled = cmdMaster.Flag.Bool("telemetry", true, "report anonymous cluster statistics to telemetry.url, use -telemetry=false to opt out")
		//   mini.go:442    miniMasterOptions.telemetryEnabled = cmdMini.Flag.Bool("master.telemetry", true, "report anonymous cluster statistics to master.telemetry.url, use -master.telemetry=false to opt out")
		"-master.telemetry=false",
	}
}

// masterArgs builds argv for `weed master` with the same privacy invariants as
// serverArgs. The loopback/telemetry flags are per-subcommand:
//   master.go:90  m.ip = cmdMaster.Flag.String("ip", ..., "master <ip>|<server> address, also used as identifier")
//   master.go:91  m.ipBind = cmdMaster.Flag.String("ip.bind", "", "ip address to bind to. If empty, default to same as -ip option.")
//   master.go:115 m.telemetryEnabled = cmdMaster.Flag.Bool("telemetry", true, ...)
func masterArgs(dataDir, addr string, port int) []string {
	return []string{
		"master",
		"-mdir=" + dataDir,
		"-ip=" + addr,
		"-ip.bind=" + addr,
		"-port=" + strconv.Itoa(port),
		"-telemetry=false",
	}
}

// miniArgs builds argv for `weed mini` (single-process mini cluster) with the
// same invariants:
//   mini.go:382  miniIp = cmdMini.Flag.String("ip", ..., "ip or server name, also used as identifier")
//   mini.go:383  miniBindIp = cmdMini.Flag.String("ip.bind", "0.0.0.0", "ip address to bind to. If empty, default to same as -ip option.")
//   mini.go:442  miniMasterOptions.telemetryEnabled = cmdMini.Flag.Bool("master.telemetry", true, ...)
func miniArgs(dataDir, addr string, port int) []string {
	return []string{
		"mini",
		"-dir=" + dataDir,
		"-ip=" + addr,
		"-ip.bind=" + addr,
		"-port=" + strconv.Itoa(port),
		"-master.telemetry=false",
	}
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
	serve.Flags().StringVar(&o.Version, "version", "", "SeaweedFS release tag (default: "+DefaultVersion+")")
	serve.Flags().StringVar(&o.DataDir, "data", "", "storage dir (default ~/.agents/bashy/seaweedfs)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "S3 gateway port")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached weed binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := resolveTool(o.Version)
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
	pathCmd.Flags().StringVar(&o.Version, "version", "", "SeaweedFS release tag (default: "+DefaultVersion+")")

	root.AddCommand(serve, pathCmd)
	return root
}
