// Package rust provisions the Rust toolchain on demand via the official
// rustup-init, so `bashy cargo`/`rustc`/`rustup` work on a bare node with no
// system Rust: binmgr fetches rustup-init (download → sha256-verify via the
// official .sha256 sidecar → cache), then runs it once with a bashy-owned
// CARGO_HOME/RUSTUP_HOME (minimal profile, no PATH modification). No embedding.
// rustup-init is from static.rust-lang.org (official); Rust is Apache-2.0/MIT.
package rust

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// IslandToolchainWindows is the rustup toolchain a Bash# Rust island uses on
// Windows (see EnsureRustc); BASHY_RUST_VERSION does not change it.
const IslandToolchainWindows = "stable-x86_64-pc-windows-gnu"

// DefaultToolchain is installed when none is requested. Override via
// $BASHY_RUST_VERSION (e.g. "1.83.0" or "stable"/"beta"/"nightly").
const DefaultToolchain = "stable"

// rustTriple maps Go's GOOS/GOARCH to Rust's host triple.
func rustTriple() (string, error) {
	var osPart string
	switch runtime.GOOS {
	case "linux":
		osPart = "unknown-linux-gnu"
	case "darwin":
		osPart = "apple-darwin"
	case "windows":
		osPart = "pc-windows-msvc"
	default:
		return "", fmt.Errorf("rust: unsupported OS %q", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", fmt.Errorf("rust: unsupported arch %q", runtime.GOARCH)
	}
	return arch + "-" + osPart, nil
}

// ensureRustupInit fetches the verified rustup-init binary.
func ensureRustupInit(ctx context.Context) (string, error) {
	triple, err := rustTriple()
	if err != nil {
		return "", err
	}
	name := "rustup-init"
	if runtime.GOOS == "windows" {
		name = "rustup-init.exe"
	}
	url := "https://static.rust-lang.org/rustup/dist/" + triple + "/" + name
	sha, err := resolveSHA(ctx, url)
	if err != nil {
		return "", err
	}
	tool := binmgr.Tool{
		Name: "rustup-init", Version: "latest",
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: url, SHA256: sha},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// resolveSHA fetches the official .sha256 sidecar — fails (never empty) so
// binmgr's fail-closed verify always has a digest.
func resolveSHA(ctx context.Context, url string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+".sha256", nil)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("rust: fetch sha256 sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("rust: sha256 sidecar HTTP %d for %s", resp.StatusCode, url)
	}
	var buf [512]byte
	n, _ := resp.Body.Read(buf[:])
	if f := strings.Fields(string(buf[:n])); len(f) > 0 && len(f[0]) == 64 {
		return f[0], nil
	}
	return "", fmt.Errorf("rust: malformed sha256 sidecar for %s", url)
}

// rustHome is the bashy-owned CARGO_HOME/RUSTUP_HOME root.
func rustHome() (cargoHome, rustupHome string, err error) {
	base, err := binmgr.CacheDir()
	if err != nil {
		return "", "", err
	}
	root := filepath.Join(filepath.Dir(base), "rust")
	return filepath.Join(root, "cargo"), filepath.Join(root, "rustup"), nil
}

// ensureToolchain installs the toolchain once (via rustup-init) and returns the
// CARGO_HOME bin dir. Idempotent: a present cargo short-circuits.
func ensureToolchain(ctx context.Context) (binDir string, env []string, err error) {
	cargoHome, rustupHome, err := rustHome()
	if err != nil {
		return "", nil, err
	}
	binDir = filepath.Join(cargoHome, "bin")
	env = append(os.Environ(), "CARGO_HOME="+cargoHome, "RUSTUP_HOME="+rustupHome,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cargo := filepath.Join(binDir, "cargo")
	if runtime.GOOS == "windows" {
		cargo += ".exe"
	}
	if _, statErr := os.Stat(cargo); statErr == nil {
		return binDir, env, nil // already installed
	}
	init, err := ensureRustupInit(ctx)
	if err != nil {
		return "", nil, err
	}
	toolchain := strings.TrimSpace(os.Getenv("BASHY_RUST_VERSION"))
	if toolchain == "" {
		toolchain = DefaultToolchain
	}
	fmt.Fprintf(os.Stderr, "note: installing the Rust toolchain (%s) via rustup — one-time, into %s\n", toolchain, cargoHome)
	c := exec.CommandContext(ctx, init, "-y", "--no-modify-path", "--profile", "minimal", "--default-toolchain", toolchain)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = env
	if runErr := c.Run(); runErr != nil {
		return "", nil, fmt.Errorf("rust: rustup-init: %w", runErr)
	}
	return binDir, env, nil
}

// NewCargoCmd / NewRustcCmd / NewRustupCmd are the `bashy cargo|rustc|rustup`
// front-doors; NewRustCmd is an alias that runs rustc.
func NewCargoCmd() *cobra.Command {
	return newCmd("cargo", "Rust build tool / package manager", "cargo")
}
func NewRustcCmd() *cobra.Command  { return newCmd("rustc", "Rust compiler", "rustc") }
func NewRustupCmd() *cobra.Command { return newCmd("rustup", "Rust toolchain manager", "rustup") }
func NewRustCmd() *cobra.Command   { return newCmd("rust", "Rust compiler (alias of rustc)", "rustc") }

func newCmd(use, short, tool string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              "Run " + short + ", auto-provisioned (download + verify + cache, no system Rust needed)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			binDir, env, err := ensureToolchain(cmd.Context())
			if err != nil {
				return err
			}
			exe := tool
			if runtime.GOOS == "windows" {
				exe += ".exe"
			}
			c := exec.CommandContext(cmd.Context(), filepath.Join(binDir, exe), args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			c.Env = env
			return c.Run()
		},
	}
}

// EnsureRustc provisions the bashy-owned Rust toolchain (rustup-init, one
// time) and returns the absolute path of the REAL rustc inside it — the
// toolchain binary, not the CARGO_HOME/bin rustup proxy, so a caller can
// exec it without RUSTUP_HOME in its environment. Idempotent.
//
// On Windows the island toolchain is the x86_64-pc-windows-GNU one: the msvc
// target needs Visual Studio's link.exe and the Windows SDK import libraries,
// which a stock host does not have, while the gnu target links through the
// provisioned zig cc (the cargo-zigbuild route) — the same on every host.
func EnsureRustc(ctx context.Context) (string, error) {
	binDir, env, err := ensureToolchain(ctx)
	if err != nil {
		return "", err
	}
	rustup := filepath.Join(binDir, "rustup")
	toolchain := []string(nil)
	if runtime.GOOS == "windows" {
		rustup += ".exe"
		toolchain = []string{"--toolchain", IslandToolchainWindows}
		c := exec.CommandContext(ctx, rustup, "toolchain", "list")
		c.Env = env
		if out, err := c.Output(); err != nil || !strings.Contains(string(out), IslandToolchainWindows) {
			fmt.Fprintf(os.Stderr, "note: installing the Rust toolchain %s via rustup — one-time\n", IslandToolchainWindows)
			c := exec.CommandContext(ctx, rustup, "toolchain", "install", IslandToolchainWindows, "--profile", "minimal")
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			c.Env = env
			if err := c.Run(); err != nil {
				return "", fmt.Errorf("rust: rustup toolchain install %s: %w", IslandToolchainWindows, err)
			}
		}
	}
	c := exec.CommandContext(ctx, rustup, append([]string{"which", "rustc"}, toolchain...)...)
	c.Env = env
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("rust: rustup which rustc: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("rust: rustup which rustc reported nothing")
	}
	return path, nil
}
