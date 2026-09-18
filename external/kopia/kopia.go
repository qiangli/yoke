// Package kopia runs Kopia (a pure-Go, content-addressed, encrypted snapshot
// backup tool) as a managed external binary (pkg/binmgr) — the backup service
// for the dhnt mesh. The kopia binary is downloaded → sha256-verified → cached by
// binmgr, never compiled in; bashy launches it via `bashy kopia`, and outpost
// runs its repository server and exposes it over the mesh as `backup` (many nodes
// back up into one repo over the forwarded port). kopia/kopia is Apache-2.0.
//
// NOTE: Kopia's repository-server flags are version-sensitive; the launch args
// below (config-file + filesystem repo + `server start --insecure
// --without-password`) target recent Kopia and should be validated on a real host
// (the structure — binmgr + external launcher + builtin — is the stable part).
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
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// DefaultVersion pins the Kopia release. "" / "latest" resolves the newest;
	// pin for reproducibility ($KOPIA_VERSION or --version override).
	DefaultVersion = "latest"
	DefaultAddr    = "127.0.0.1"
	DefaultPort    = 51515 // Kopia's default server port
)

// Spec is the binmgr GitHub spec. Kopia ships per-platform archives whose binary
// is nested in a versioned subdir (matched by basename), and uses non-Go os/arch
// tokens (macOS, x64) — handled by the custom AssetMatch.
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name: "kopia", Repo: "kopia/kopia", Version: version,
		Member: "kopia", AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goos/goarch onto Kopia's release tokens (darwin→macos,
// amd64→x64) and matches the archive for the current platform.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	osTok := goos
	if goos == "darwin" {
		osTok = "macos"
	}
	if !strings.Contains(n, osTok) {
		return false
	}
	archTok := goarch
	if goarch == "amd64" {
		archTok = "x64"
	}
	return strings.Contains(n, archTok)
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
	tool, err := binmgr.ResolveGitHub(ctx, Spec(o.Version))
	if err != nil {
		return nil, fmt.Errorf("kopia: resolve: %w", err)
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
		Args: []string{
			"--config-file=" + cfg,
			"server", "start",
			"--address=" + addr,
			"--insecure",         // HTTP, no TLS (the mesh is the boundary)
			"--without-password", // no server-level auth (loopback + mesh-gated)
		},
		Env:        []string{"KOPIA_PASSWORD=" + pw},
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
		Args:   []string{"--config-file=" + cfg, "repository", "create", "filesystem", "--path=" + repoPath},
		Env:    []string{"KOPIA_PASSWORD=" + pw},
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
	serve.Flags().StringVar(&o.Version, "version", "", "Kopia release tag (default: latest)")
	serve.Flags().StringVar(&o.DataDir, "data", "", "home/repo dir (default ~/.agents/bashy/kopia)")
	serve.Flags().StringVar(&o.Addr, "addr", DefaultAddr, "bind address")
	serve.Flags().IntVar(&o.Port, "port", DefaultPort, "server port")

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print the cached kopia binary path (fetching it if needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			tool, err := binmgr.ResolveGitHub(cmd.Context(), Spec(o.Version))
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
	pathCmd.Flags().StringVar(&o.Version, "version", "", "Kopia release tag (default: latest)")

	root.AddCommand(serve, pathCmd)
	return root
}
