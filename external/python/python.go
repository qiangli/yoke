// Package python provisions the Python toolchain on demand via astral-sh/uv —
// a single self-contained binary that itself downloads + manages CPython. So
// `bashy python`/`pip`/`uv` all work on a bare node with no system Python:
// binmgr fetches uv (download → sha256-verify → cache), then uv provisions
// CPython on first `python`/`pip` use. No embedding. uv is Apache-2.0/MIT.
package python

import (
	"bufio"
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

// DefaultVersion pins the uv release fetched when none is requested. uv is
// backward-compatible; bump periodically or override with $BASHY_UV_VERSION.
const DefaultVersion = "0.5.11"

// uvTriple maps Go's GOOS/GOARCH to uv's release triple + archive ext.
func uvTriple() (triple, ext string, err error) {
	var osPart string
	switch runtime.GOOS {
	case "linux":
		osPart = "unknown-linux-gnu"
	case "darwin":
		osPart = "apple-darwin"
	case "windows":
		osPart = "pc-windows-msvc"
	default:
		return "", "", fmt.Errorf("python/uv: unsupported OS %q", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	default:
		return "", "", fmt.Errorf("python/uv: unsupported arch %q", runtime.GOARCH)
	}
	ext = ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	return arch + "-" + osPart, ext, nil
}

// EnsureUv fetches (if needed) the uv binary and returns its cached path.
func EnsureUv(ctx context.Context, version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		version = DefaultVersion
	}
	if version != DefaultVersion {
		fmt.Fprintf(os.Stderr, "note: uv %s is not the pinned default (%s) — verifying against the official published checksum\n", version, DefaultVersion)
	}
	triple, ext, err := uvTriple()
	if err != nil {
		return "", err
	}
	// The tarball extracts to uv-<triple>/{uv,uvx}; the Windows zip extracts
	// uv.exe at the root.
	entry := "uv-" + triple + "/uv"
	if runtime.GOOS == "windows" {
		entry = "uv.exe"
	}
	probe := binmgr.Tool{
		Name: "uv", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: "cached", Tree: true, Entrypoint: entry},
		},
	}
	if p, perr := binmgr.Ensure(ctx, probe); perr == nil {
		return p, nil
	}
	filename := "uv-" + triple + ext
	url := "https://github.com/astral-sh/uv/releases/download/" + version + "/" + filename
	sha, err := resolveSHA(ctx, url)
	if err != nil {
		return "", err
	}
	tool := binmgr.Tool{
		Name: "uv", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: url, SHA256: sha, Tree: true, Entrypoint: entry},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// resolveSHA fetches the asset's .sha256 sidecar (astral publishes one per
// asset) and returns the digest. Fails — never returns empty — so binmgr's
// fail-closed verify always has a checksum (no unverified install).
func resolveSHA(ctx context.Context, assetURL string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, assetURL+".sha256", nil)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("python/uv: fetch sha256 sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("python/uv: sha256 sidecar HTTP %d for %s (version not found?)", resp.StatusCode, assetURL)
	}
	sc := bufio.NewScanner(resp.Body)
	if sc.Scan() {
		// Line is "<sha>  <filename>" (or just the hex).
		if f := strings.Fields(sc.Text()); len(f) > 0 {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("python/uv: empty sha256 sidecar for %s", assetURL)
}

// run provisions uv then execs a uv-managed tool. python -> `uv run python`,
// pip -> `uv pip`, uv -> uv itself.
func run(ctx context.Context, mode string, args []string) error {
	uv, err := EnsureUv(ctx, os.Getenv("BASHY_UV_VERSION"))
	if err != nil {
		return err
	}
	binDir := filepath.Dir(uv)
	var argv []string
	switch mode {
	case "uv":
		argv = args
	case "python":
		argv = append([]string{"run", "python"}, args...)
	case "pip":
		argv = append([]string{"pip"}, args...)
	}
	c := exec.CommandContext(ctx, uv, argv...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	c.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return c.Run()
}

// NewUvCmd / NewPythonCmd / NewPipCmd are the `bashy uv|python|pip` front-doors.
func NewUvCmd() *cobra.Command     { return newCmd("uv", "uv (Python package/project manager)", "uv") }
func NewPythonCmd() *cobra.Command { return newCmd("python", "Python interpreter via uv", "python") }
func NewPipCmd() *cobra.Command    { return newCmd("pip", "Python package installer via uv", "pip") }

func newCmd(use, short, mode string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              "Run " + short + ", auto-provisioned (download + verify + cache, no system Python needed)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd.Context(), mode, args)
		},
	}
}
