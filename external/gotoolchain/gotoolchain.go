// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package gotoolchain provisions an official Go toolchain on demand so `bashy
// go` works on a bare node with no system Go: resolve the platform archive from
// go.dev's release index (for the sha256 — go has no .sha256 sidecar), then hand
// off to binmgr's tree-mode Ensure (download → verify → extract the whole GOROOT
// → cache → exec). No embedding. This is the self-sufficient worker story.
package gotoolchain

import (
	"context"
	"encoding/json"
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

// DefaultVersion is used when no version is requested. Keep in step with the
// Bash# Go coordinate: fenced ~~~go islands and --source=go refuse a toolchain
// below 1.27, so the toolchain bashy provisions for itself must satisfy them.
const DefaultVersion = "1.27.1"

const releaseIndexURL = "https://go.dev/dl/?mode=json&include=all"

type goFile struct {
	Filename string `json:"filename"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kind     string `json:"kind"` // "archive" | "installer" | "source"
	SHA256   string `json:"sha256"`
}

type goRelease struct {
	Version string   `json:"version"` // e.g. "go1.26.4"
	Files   []goFile `json:"files"`
}

func entrypoint() string {
	if runtime.GOOS == "windows" {
		return "go/bin/go.exe"
	}
	return "go/bin/go"
}

// Ensure makes the requested Go toolchain available and returns the path to its
// `go` executable plus its GOROOT. Idempotent: a cache hit does no network I/O
// (binmgr short-circuits on the cached entrypoint). version is a bare number
// like "1.27.1" (a leading "go" is tolerated); empty means DefaultVersion.
func Ensure(ctx context.Context, version string) (goBin, goroot string, err error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "go")
	if version == "" {
		version = DefaultVersion
	}
	// Fast path: if binmgr already has the tree cached we can skip the network
	// round-trip to the release index entirely. Probe with an empty-sha asset;
	// binmgr returns the entrypoint on a cache hit before any download.
	probe := binmgr.Tool{
		Name: "go", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {URL: "cached", Tree: true, Entrypoint: entrypoint()},
		},
	}
	if p, perr := binmgr.Ensure(ctx, probe); perr == nil {
		return p, filepath.Dir(filepath.Dir(p)), nil
	}

	file, err := resolveFile(ctx, version)
	if err != nil {
		return "", "", err
	}
	tool := binmgr.Tool{
		Name: "go", Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {
				URL:        "https://go.dev/dl/" + file.Filename,
				SHA256:     file.SHA256,
				Tree:       true,
				Entrypoint: entrypoint(),
			},
		},
	}
	goBin, err = binmgr.Ensure(ctx, tool)
	if err != nil {
		return "", "", err
	}
	// entrypoint is <cache>/go/<ver>/go/bin/go → GOROOT is <cache>/go/<ver>/go.
	return goBin, filepath.Dir(filepath.Dir(goBin)), nil
}

// resolveFile fetches go.dev's release index and finds the archive for the
// requested version and the current platform, returning its filename + sha256.
func resolveFile(ctx context.Context, version string) (goFile, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, releaseIndexURL, nil)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return goFile{}, fmt.Errorf("gotoolchain: fetch release index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return goFile{}, fmt.Errorf("gotoolchain: release index HTTP %d", resp.StatusCode)
	}
	var releases []goRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return goFile{}, fmt.Errorf("gotoolchain: decode release index: %w", err)
	}
	want := "go" + version
	for _, r := range releases {
		if r.Version != want {
			continue
		}
		for _, f := range r.Files {
			if f.Kind == "archive" && f.OS == runtime.GOOS && f.Arch == runtime.GOARCH {
				return f, nil
			}
		}
		return goFile{}, fmt.Errorf("gotoolchain: go%s has no archive for %s/%s", version, runtime.GOOS, runtime.GOARCH)
	}
	return goFile{}, fmt.Errorf("gotoolchain: version go%s not found in release index", version)
}

// NewGoCmd is the `bashy go` front-door: ensure the toolchain (version from
// $BASHY_GO_VERSION or the default), then exec it with the user's args and a
// pinned GOROOT. Flags are passed through untouched to the real go.
func NewGoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "go",
		Short:              "Run the Go toolchain, auto-provisioned (download + cache, no system Go needed)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			goBin, goroot, err := Ensure(cmd.Context(), os.Getenv("BASHY_GO_VERSION"))
			if err != nil {
				return err
			}
			c := exec.CommandContext(cmd.Context(), goBin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			c.Env = goCommandEnv(goroot)
			return c.Run()
		},
	}
	return cmd
}

func goCommandEnv(goroot string) []string {
	env := append([]string{}, os.Environ()...)
	if runtime.GOOS == "windows" {
		env = normalizeWindowsTempEnv(env)
	}
	return append(env, "GOROOT="+goroot)
}

func normalizeWindowsTempEnv(env []string) []string {
	out := make([]string, 0, len(env)+2)
	seen := map[string]bool{}
	for _, e := range env {
		name, value, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		upper := strings.ToUpper(name)
		switch upper {
		case "TMP", "TEMP", "TMPDIR", "GOPATH", "GOMODCACHE", "GOCACHE", "GOENV", "GOROOT", "HOME", "USERPROFILE":
			value = windowsNativePath(value)
			seen[upper] = true
		}
		out = append(out, name+"="+value)
	}
	fallback := windowsNativePath(os.TempDir())
	if fallback != "" {
		if !seen["TMP"] {
			out = append(out, "TMP="+fallback)
		}
		if !seen["TEMP"] {
			out = append(out, "TEMP="+fallback)
		}
	}
	return out
}

func windowsNativePath(path string) string {
	return windowsNativePathForGOOS(runtime.GOOS, path)
}

func windowsNativePathForGOOS(goos, path string) string {
	if goos != "windows" || path == "" {
		return path
	}
	p := strings.ReplaceAll(path, `/`, `\`)
	if len(p) >= 2 && p[0] == '\\' && asciiLetter(p[1]) && (len(p) == 2 || p[2] == '\\') {
		drive := p[1]
		if 'a' <= drive && drive <= 'z' {
			drive -= 'a' - 'A'
		}
		rest := p[2:]
		if rest == "" {
			rest = `\`
		}
		return string(drive) + ":" + rest
	}
	return p
}

func asciiLetter(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}
