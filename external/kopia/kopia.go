// Package kopia runs Kopia (a pure-Go, content-addressed, encrypted snapshot
// backup tool) as a managed external binary (pkg/binmgr) — the backup service
// for the dhnt mesh. The kopia binary is downloaded → sha256-verified → cached by
// binmgr, never compiled in; bashy launches it via `bashy kopia`, and outpost
// runs its repository server and exposes it over the mesh as `backup` (many nodes
// back up into one repo over the forwarded port). Upstream kopia/kopia is
// Apache-2.0; the bashy fork qiangli/kopia is the signed supply-chain pin.
// See dhnt/docs/external-binary-builtins.md + docs/mesh-wrap-stack.md.
package kopia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/internal/serviceenv"
	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// Upstream release that the bashy fork was built from (builds/kopia at the
	// pinned commit). The fork tag is the upstream tag plus the bashy build counter.
	kopiaVersion = "v0.23.1"
	// Bashy-signed fork release tag (builds/kopia/tool.env at the pin):
	// built by the bashy-build workflow on qiangli/kopia and published as
	// raw executables with per-asset SHA256SUMS and <asset>.sigstore.json bundles.
	kopiaBashyTag = "bashy-v0.23.1-3"

	// DefaultVersion is the pinned bashy tag. There is no "latest" — that was
	// the upstream archive and it is intentionally absent (fail closed).
	DefaultVersion = kopiaBashyTag
	DefaultAddr    = "127.0.0.1"
	DefaultPort    = 51515 // Kopia's default server port
)

// SHA256 per asset read from the published SHA256SUMS of the bashy release
// (downloaded via curl -sL, not from the GitHub API asset digest).
var kopiaSHAs = map[string]string{
	"linux/amd64":  "6ae24fa8ca2b885e5a9c169b413d804681e4617413418ade96fc6e957605a89a",
	"linux/arm64":  "5c06ddbd69a7bf38c940137110f15e150341c9349822e8ac0f953a6492504454",
	"darwin/amd64": "05e0de6ebb38a76d71f9617440714791f65e1a3eb5395cb0dcc14c049cf02eeb",
	"darwin/arm64": "0de1ec8a291438f7978eca197cb37ee5a33a090179c5d3dcbae2358adbc5fefa",
}

func kopiaTool() binmgr.Tool {
	assets := map[string]binmgr.Asset{}
	for plat, sha := range kopiaSHAs {
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := "kopia-" + goos + "-" + goarch
		url := fmt.Sprintf("https://github.com/qiangli/kopia/releases/download/%s/%s", kopiaBashyTag, assetName)
		assets[plat] = binmgr.Asset{URL: url, SHA256: sha}
	}
	identity := fmt.Sprintf("https://github.com/qiangli/kopia/.github/workflows/bashy-build.yml@refs/tags/%s", kopiaBashyTag)
	return binmgr.Tool{
		Name:    "kopia",
		Version: kopiaBashyTag,
		Assets:  assets,
		Sigstore: &binmgr.Sigstore{
			Identity: identity,
			Issuer:   binmgr.GitHubActionsIssuer,
		},
	}
}

// KopiaTool is the pinned, Sigstore-verified tool declaration. Exported
// for tests and for callers that need to inspect the pin without launching.
var KopiaTool = kopiaTool()

// Spec is retained for compatibility with callers that probe the resolver's
// asset matcher (e.g. pkg/atlas/platform_external_test). It returns a
// GitHubSpec for the qiangli fork, not the upstream kopia/kopia archive,
// and never returns "latest".
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	if version == kopiaVersion {
		version = kopiaBashyTag
	}
	return binmgr.GitHubSpec{Name: "kopia", Repo: "qiangli/kopia", Version: version, AssetMatch: assetMatch}
}

func assetMatch(name, goos, goarch string) bool {
	return strings.EqualFold(name, "kopia-"+goos+"-"+goarch)
}

func resolveTool(version string) (binmgr.Tool, error) {
	if runtime.GOOS == "windows" {
		return binmgr.Tool{}, fmt.Errorf("kopia: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed")
	}
	tool := KopiaTool
	switch version {
	case "", kopiaBashyTag, kopiaVersion:
		return tool, nil
	default:
		return binmgr.Tool{}, fmt.Errorf("kopia: version %q not supported: only %q (bashy-signed) is available (upstream %q alias accepted)", version, kopiaBashyTag, kopiaVersion)
	}
}

// DefaultDataDir is Kopia's home (config + the filesystem repository).
func DefaultDataDir() string {
	if d := os.Getenv("KOPIA_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "kopia")
	}
	return filepath.Join(home, ".agents", "bashy", "kopia")
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

// Instance is a running Kopia repository server.
type Instance struct {
	URL     string
	Addr    string
	Version string
	proc    *binmgr.Process
}

// Stop terminates the kopia server gracefully.
func (i *Instance) Stop() error {
	if i == nil || i.proc == nil {
		return nil
	}
	return i.proc.Stop(10 * time.Second)
}

// Start resolves + launches `kopia server start` (NON-blocking), creating the
// filesystem repository on first run, and returns once the server answers. The
// caller owns the Instance — outpost's wrap-harness builtin supervises it and
// exposes it over the mesh.
func Start(ctx context.Context, o Options) (*Instance, error) {
	o.defaults()
	if runtime.GOOS == "windows" {
		return nil, fmt.Errorf("kopia: supply-chain policy: no bashy-signed asset for windows (no upstream fallback) — only linux and darwin assets are signed")
	}
	tool, err := resolveTool(o.Version)
	if err != nil {
		return nil, err
	}
	bin, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return nil, fmt.Errorf("kopia: fetch: %w", err)
	}
	cfg, pw, err := ensureRepo(ctx, bin, o.DataDir)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("%s:%d", o.Addr, o.Port)
	url := "http://" + addr
	proc, err := binmgr.Launch(ctx, bin, binmgr.RunSpec{
		Args:       serverArgs(cfg, addr),
		Env:        kopiaEnv(pw),
		Dir:        o.DataDir,
		Stdout:     o.Stdout,
		Stderr:     o.Stderr,
		HealthURL:  url + "/",
		HealthWait: 30 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("kopia: start server: %w", err)
	}
	return &Instance{URL: url, Addr: addr, Version: tool.Version, proc: proc}, nil
}

// kopiaPrivacyArgs returns the global flags shared by every kopia invocation.
// --no-persist-credentials is a global app flag (cli/app.go at v0.23.1:
//   app.Flag("persist-credentials" … Default("true") …), whose negated form
//   is --no-persist-credentials; strategy passwordPersistenceStrategy at
//   cli/app.go:234-246 selects Keychain+File vs None, with Darwin defaulting
//   to Keychain at cli/password_darwin.go). Without it, KOPIA_PASSWORD
//   triggers persistence to the OS keychain on macOS and blocks headless
//   operation (the server never listens until the Keychain prompt is
//   dismissed). See bashy builds/kopia/files/no-egress-test.sh at 98300aeb
//   and builds/kopia/README.md for the headless gate.
// --otlp-trace is also a global app flag (cli/observability_flags.go at v0.23.1),
// so both are valid before any subcommand. --no-check-for-updates is NOT: it
// only exists on `repository connect/create`, and `server start` rejects it.
// Update checks are disabled through the environment instead (see kopiaEnv).
func kopiaPrivacyArgs() []string {
	return []string{"--no-persist-credentials", "--otlp-trace"}
}

// serverArgs is the argv for `kopia server start`.
func serverArgs(cfg, addr string) []string {
	return append(kopiaPrivacyArgs(),
		"--config-file="+cfg,
		"server", "start",
		"--address="+addr,
		"--insecure",         // HTTP, no TLS (the mesh is the boundary)
		"--without-password", // no server-level auth (loopback + mesh-gated)
	)
}

// kopiaEnv is the managed child environment. KOPIA_CHECK_FOR_UPDATES=false is
// the variable cli/update_check.go reads (checkForUpdatesEnvar, resolved via
// App.EnvName, whose prefix is empty outside tests) and the Envar of the
// connect/create --check-for-updates flag, so it covers server and repo create.
func kopiaEnv(pw string) []string {
	return serviceenv.LocalOTLP([]string{"KOPIA_PASSWORD=" + pw, "KOPIA_CHECK_FOR_UPDATES=false"}, "kopia")
}

// Serve runs Kopia's repository server in the foreground (the `bashy kopia serve`
// path), blocking until ctx is cancelled or a signal arrives.
func Serve(ctx context.Context, o Options) error {
	o.defaults()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	inst, err := Start(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Stdout, "kopia (%s) repository server on %s — repo %s\n", inst.Version, inst.URL, o.DataDir)
	fmt.Fprintln(o.Stdout, "expose it over the mesh:  outpost mesh service add backup "+inst.Addr)

	<-ctx.Done()
	fmt.Fprintln(o.Stdout, "kopia: shutting down…")
	return inst.Stop()
}

// ensureRepo creates the filesystem repository on first run (generating + storing
// its password) and returns the config-file path + the repo password. On
// subsequent runs it reuses the stored password (the repo is already connected in
// the config file).
func ensureRepo(ctx context.Context, bin, dataDir string) (cfg, pw string, err error) {
	if err = os.MkdirAll(dataDir, 0o755); err != nil {
		return "", "", err
	}
	cfg = filepath.Join(dataDir, "kopia.config")
	pwFile := filepath.Join(dataDir, "repo-password")
	if b, e := os.ReadFile(pwFile); e == nil {
		return cfg, strings.TrimSpace(string(b)), nil // already set up
	}
	pw, err = randomHex(24)
	if err != nil {
		return "", "", err
	}
	repoPath := filepath.Join(dataDir, "repository")
	if err = os.MkdirAll(repoPath, 0o755); err != nil {
		return "", "", err
	}
	proc, err := binmgr.Launch(ctx, bin, binmgr.RunSpec{
		Args:   append(kopiaPrivacyArgs(), "--config-file="+cfg, "repository", "create", "filesystem", "--path="+repoPath),
		Env:    kopiaEnv(pw),
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		return cfg, "", fmt.Errorf("kopia: launch repo create: %w", err)
	}
	if err = proc.Wait(); err != nil {
		return cfg, "", fmt.Errorf("kopia: repository create: %w", err)
	}
	if err = os.WriteFile(pwFile, []byte(pw), 0o600); err != nil {
		return cfg, "", err
	}
	return cfg, pw, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewKopiaCmd builds the `kopia` command tree (bashy front-door).
func NewKopiaCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "kopia",
		Short: "Run Kopia (the mesh snapshot-backup repository server) as a managed external binary",
		Long: `kopia runs the Kopia repository server — downloaded, sha256-verified, and
cached by binmgr (not compiled in). Expose it over the mesh with:
outpost mesh service add backup <addr>. Many nodes back up into one repo.`,
	}
	var o Options
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Download (if needed) + run the Kopia repository server on a loopback port",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Serve(cmd.Context(), o)
		},
	}
	serve.Flags().StringVar(&o.Version, "version", "", "Kopia release tag (default: "+DefaultVersion+")")
	serve.Flags().StringVar(&o.DataDir, "data", "", "home/repo dir (default ~/.agents/bashy/kopia)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "server port")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached kopia binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := resolveTool(o.Version)
			if err != nil {
				return err
			}
			p, err := binmgr.Ensure(cmd.Context(), tool)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t(kopia %s)\n", p, tool.Version)
			return nil
		},
	}
	pathCmd.Flags().StringVar(&o.Version, "version", "", "Kopia release tag (default: "+DefaultVersion+")")

	root.AddCommand(serve, pathCmd)
	return root
}
