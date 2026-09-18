// Package winhelper provisions the helper binaries podman needs on
// Windows, and exports the environment that lets podman find them.
//
// It is deliberately SEPARATE from external/podman/engine, and imports
// only pkg/binmgr plus the stdlib, because the callers that need it most
// cannot link the engine. bashy's shipped Windows build is the lean
// worker: the in-process libpod engine sits behind `-tags bashy_engines`
// and is excluded, so `bashy podman …` resolves a podman binary and execs
// it (the "exec, never link" dispatch ladder). That exec path had no way
// to reach the provisioning that lived inside the engine package.
//
// Why this matters at all: on Windows, podman does not serve its API
// itself. `podman machine start` launches win-sshproxy.exe, which
// forwards the machine's unix socket onto `\\.\pipe\podman-<machine>`,
// `\\.\pipe\docker_engine`, and %TEMP%\podman\<machine>-api.sock. podman
// locates that helper through containers.conf's helper_binaries_dir or
// $CONTAINERS_HELPER_BINARY_DIR. When it finds neither, the start still
// SUCCEEDS — it prints "API forwarding ... not available" and publishes
// NO endpoint at all. The VM runs and nothing can talk to it, which is
// indistinguishable from "podman is not installed" to any client probing
// for a socket.
package winhelper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// EnvVar is the variable podman's config.FindHelperBinary prepends to its
// helper search list. Setting it is what makes a provisioned helper
// discoverable without editing containers.conf.
const EnvVar = "CONTAINERS_HELPER_BINARY_DIR"

// gvisorTapVsockVersion pins the release the helpers come from.
// gvproxy and win-sshproxy ship together from containers/gvisor-tap-vsock
// (Apache-2.0), downloaded + sha256-verified by binmgr.
const gvisorTapVsockVersion = "v0.8.8"

// Asset describes one helper binary to stage into the cache dir.
type Asset struct {
	Name       string // binmgr tool name
	AssetName  string // release asset filename
	SHA256     string // pinned digest — binmgr fails closed without a match
	StagedName string // filename podman looks for in the helper dir
}

// Assets returns the helper set for the running Windows architecture.
func Assets() ([]Asset, error) {
	switch runtime.GOARCH {
	case "amd64":
		return []Asset{
			{
				Name:       "gvproxy",
				AssetName:  "gvproxy-windowsgui.exe",
				SHA256:     "8803caf895325dc2ea52337fa2c7c835c1f7f115b0bde71fdb1479d1b3710526",
				StagedName: "gvproxy.exe",
			},
			{
				Name:       "win-sshproxy",
				AssetName:  "win-sshproxy.exe",
				SHA256:     "afa4c0d97787f2a4e6509cfe472e9d2ceb5fcfd41a870e66687aa314909b4d10",
				StagedName: "win-sshproxy.exe",
			},
		}, nil
	case "arm64":
		return []Asset{
			{
				Name:       "gvproxy",
				AssetName:  "gvproxy-windows-arm64.exe",
				SHA256:     "c2ee761781e58604438b2686531ba2572dce4933f2a4cbccf5da79247bc93412",
				StagedName: "gvproxy.exe",
			},
			{
				Name:       "win-sshproxy",
				AssetName:  "win-sshproxy-arm64.exe",
				SHA256:     "f38633a252a8916342db95f697d0f992a7494e0e74cf11e6e7432892d7fa0916",
				StagedName: "win-sshproxy.exe",
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported Windows architecture %s", runtime.GOARCH)
	}
}

// Ensure downloads (or reuses) each helper and stages it into cacheDir
// under the filename podman expects. A no-op off Windows.
//
// Idempotent: binmgr caches by name+version+digest, and the staging copy
// is skipped when an identically-sized file is already in place, so a
// repeated call costs a stat rather than a download.
func Ensure(cacheDir string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	assets, err := Assets()
	if err != nil {
		return err
	}
	for _, a := range assets {
		path, err := binmgr.Ensure(context.Background(), binmgr.Tool{
			Name:    a.Name,
			Version: gvisorTapVsockVersion,
			Assets: map[string]binmgr.Asset{
				binmgr.Platform(): {
					URL:    "https://github.com/containers/gvisor-tap-vsock/releases/download/" + gvisorTapVsockVersion + "/" + a.AssetName,
					SHA256: a.SHA256,
				},
			},
		})
		if err != nil {
			return fmt.Errorf("ensure %s: %w", a.StagedName, err)
		}
		if err := stageFile(path, filepath.Join(cacheDir, a.StagedName)); err != nil {
			return fmt.Errorf("stage %s: %w", a.StagedName, err)
		}
	}
	return nil
}

// Apply provisions the helpers and exports the environment podman reads
// to find them: $CONTAINERS_HELPER_BINARY_DIR for the helper_binaries_dir
// search, plus cacheDir on PATH for helpers looked up that way instead.
//
// Both are set on the CURRENT process so an in-process machine start sees
// them and any child inherits them via os.Environ(). An operator's
// existing $CONTAINERS_HELPER_BINARY_DIR is left alone.
//
// Returns nil off Windows, and an error only when provisioning failed —
// callers should log it and continue, since a podman that is already
// configured with a helper dir works regardless.
func Apply(cacheDir string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	if err := Ensure(cacheDir); err != nil {
		return err
	}
	if os.Getenv(EnvVar) == "" {
		_ = os.Setenv(EnvVar, cacheDir)
	}
	if p := os.Getenv("PATH"); !strings.Contains(p, cacheDir) {
		_ = os.Setenv("PATH", cacheDir+string(os.PathListSeparator)+p)
	}
	return nil
}

// stageFile copies src to dest with an executable mode, skipping the copy
// when dest already has the same size (the helpers are digest-pinned, so
// size equality is a sufficient staleness check here).
func stageFile(src, dest string) error {
	si, err := os.Stat(src)
	if err != nil {
		return err
	}
	if di, err := os.Stat(dest); err == nil && di.Size() == si.Size() {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.OpenFile(dest+".tmp", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	// Windows refuses a rename onto an existing file.
	_ = os.Remove(dest)
	return os.Rename(tmp.Name(), dest)
}
