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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/artifact"
	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/gomod"
)

// DefaultVersion is used when no version is requested. Keep in step with the
// Bash# Go coordinate: fenced ~~~go islands and --source=go refuse a toolchain
// below 1.27, so the toolchain bashy provisions for itself must satisfy them.
// Keep in step with the product go.mod toolchain (go1.27.4): the official
// build image pre-seeds exactly this version in its binmgr cache, and asking
// for any other version misses that cache (story 389edc3a9dc8).
const DefaultVersion = "1.27.4"

// ProxyURLEnv passes an already-started loopback CAS Go module proxy URL to
// nested child `bashy go` invocations (such as productgo) so they reuse the
// same ephemeral proxy without contending on catalog.db.
const ProxyURLEnv = "BASHY_GOMOD_PROXY_URL"

// LogicalNameToolchain is the artifact catalog logical name for Go toolchain seeds.
const LogicalNameToolchain = "gotoolchain/go"

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

func restoreToolchainFromCAS(ctx context.Context, artRoot, version string) error {
	if strings.TrimSpace(artRoot) == "" {
		artRoot = artifact.DefaultRoot()
	}
	var backend artifact.Backend
	if client, ok := artifact.TryConnectService(ctx, artRoot); ok {
		backend = client
	} else {
		dbPath := filepath.Join(artRoot, "catalog.db")
		fi, err := os.Stat(dbPath)
		if err != nil || fi.IsDir() {
			return artifact.ErrNotFound
		}
		store, err := artifact.OpenStore(ctx, artifact.StoreOptions{Root: artRoot, ReadOnly: true})
		if err != nil {
			return err
		}
		backend = store
	}
	defer backend.Close()

	st, err := backend.LookupLogical(ctx, LogicalNameToolchain, version)
	if err != nil {
		return err
	}
	if !st.PayloadPresent {
		return artifact.ErrPayloadEvicted
	}

	rc, err := backend.OpenBlob(ctx, st.Record.Digest)
	if err != nil {
		return err
	}
	defer rc.Close()

	tmpTar, err := os.CreateTemp("", "bashy-gotoolchain-seed-*.tar")
	if err != nil {
		return err
	}
	tmpName := tmpTar.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmpTar, rc); err != nil {
		_ = tmpTar.Close()
		return err
	}
	if err := tmpTar.Close(); err != nil {
		return err
	}
	return binmgr.ImportSeed(ctx, tmpName)
}

// Ensure makes the requested Go toolchain available and returns the path to its
// `go` executable plus its GOROOT. Idempotent: a cache hit does no network I/O
// (binmgr short-circuits on the cached entrypoint). version is a bare number
// like "1.27.4" (a leading "go" is tolerated); empty means $BASHY_GO_VERSION
// when set (the same selection the `bashy go` front door makes), else
// DefaultVersion.
func Ensure(ctx context.Context, version string) (goBin, goroot string, err error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "go")
	if version == "" {
		version = strings.TrimPrefix(strings.TrimSpace(os.Getenv("BASHY_GO_VERSION")), "go")
	}
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

	// Sealed / offline mode: attempt verified restoration from local artifact CAS,
	// and fail closed without ever contacting go.dev if uncached.
	if artifact.IsSealed("") || binmgr.Offline() {
		if rerr := restoreToolchainFromCAS(ctx, "", version); rerr == nil {
			if p, perr := binmgr.Ensure(ctx, probe); perr == nil {
				return p, filepath.Dir(filepath.Dir(p)), nil
			}
		} else if !errors.Is(rerr, artifact.ErrNotFound) && !errors.Is(rerr, artifact.ErrPayloadEvicted) {
			return "", "", fmt.Errorf("%w: restore go%s toolchain from artifact store: %v", artifact.ErrSealedNetworkDenied, version, rerr)
		}
		return "", "", fmt.Errorf("%w: go%s toolchain not in local artifact store", artifact.ErrSealedNetworkDenied, version)
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

func isLoopbackHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") || u.Host == "" {
		return false
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SanitizeSealedGOPROXY strips direct, https://, and non-loopback http:// entries
// from a GOPROXY list, preserving only explicit "off", "file://...", or loopback
// "http://127.0.0.1:..." / "http://[::1]:..." entries.
func SanitizeSealedGOPROXY(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if trimmed == "off" {
		return "off"
	}
	fields := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ',' || r == '|'
	})
	var kept []string
	for _, tok := range fields {
		tok = strings.TrimSpace(tok)
		if tok == "" || tok == "direct" {
			continue
		}
		if tok == "off" || strings.HasPrefix(tok, "file://") || isLoopbackHTTPURL(tok) {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, ",")
}

func lookupEnvInSlice(env []string, key string) (string, bool) {
	prefix := key + "="
	val := ""
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
			found = true
		}
	}
	return val, found
}

func setEnvKeyValues(env []string, dropKeys map[string]bool, overrides []string) []string {
	overrideKeys := make(map[string]bool, len(overrides))
	for _, kv := range overrides {
		k, _, _ := strings.Cut(kv, "=")
		overrideKeys[k] = true
	}
	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if dropKeys[k] || overrideKeys[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, overrides...)
}

// SealedCommandEnv constructs the hermetic Go command environment when sealed:
// pins GOROOT, forces GOTOOLCHAIN=local, GOSUMDB=off, GONOPROXY=, GOPRIVATE=,
// BASHY_OFFLINE=1, strips external proxy env vars and non-loopback GOPROXY
// fallbacks, and starts an ephemeral loopback CAS Go module proxy when needed.
func SealedCommandEnv(ctx context.Context, baseEnv []string, goroot string, artRoot string) ([]string, func(), error) {
	noop := func() {}
	env := append([]string{}, baseEnv...)
	if runtime.GOOS == "windows" {
		env = normalizeWindowsTempEnv(env)
	}
	env = PinGOROOT(env, goroot)
	env = notrack.Env(env)

	rawGoProxy, _ := lookupEnvInSlice(baseEnv, "GOPROXY")
	explicitProxyURL, _ := lookupEnvInSlice(baseEnv, ProxyURLEnv)

	if strings.TrimSpace(artRoot) == "" {
		if envRoot, ok := lookupEnvInSlice(baseEnv, "BASHY_ARTIFACTS_DIR"); ok && strings.TrimSpace(envRoot) != "" {
			artRoot = strings.TrimSpace(envRoot)
		} else {
			artRoot = artifact.DefaultRoot()
		}
	}

	var goproxyVal string
	var proxyExport string
	cleanup := noop

	switch {
	case strings.TrimSpace(rawGoProxy) == "off":
		goproxyVal = "off"
	case isLoopbackHTTPURL(explicitProxyURL):
		goproxyVal = strings.TrimSpace(explicitProxyURL)
		proxyExport = goproxyVal
	default:
		if sanitized := SanitizeSealedGOPROXY(rawGoProxy); sanitized != "" {
			goproxyVal = sanitized
			if isLoopbackHTTPURL(sanitized) {
				proxyExport = sanitized
			}
		} else {
			dbPath := filepath.Join(artRoot, "catalog.db")
			if fi, err := os.Stat(dbPath); err == nil && !fi.IsDir() {
				var loomDir string
				if envLoom, ok := lookupEnvInSlice(baseEnv, gomod.LoomEnv); ok && strings.TrimSpace(envLoom) != "" {
					loomDir = strings.TrimSpace(envLoom)
				}
				srv, err := gomod.StartProxyServer(ctx, gomod.ProxyServerOptions{
					Addr:         "127.0.0.1:0",
					ArtifactRoot: artRoot,
					LoomRepoDir:  loomDir,
					Sealed:       true,
				})
				if err != nil {
					return nil, nil, err
				}
				goproxyVal = srv.URL()
				proxyExport = srv.URL()
				cleanup = func() { _ = srv.Close() }
			} else {
				goproxyVal = "off"
			}
		}
	}

	dropHTTPProxies := map[string]bool{
		"HTTP_PROXY":  true,
		"http_proxy":  true,
		"HTTPS_PROXY": true,
		"https_proxy": true,
		"ALL_PROXY":   true,
		"all_proxy":   true,
	}
	overrides := []string{
		"GOPROXY=" + goproxyVal,
		"GOTOOLCHAIN=local",
		"GOSUMDB=off",
		"GONOPROXY=",
		"GOPRIVATE=",
		"BASHY_OFFLINE=1",
		"NO_PROXY=*",
		"no_proxy=*",
	}
	if proxyExport != "" {
		overrides = append(overrides, ProxyURLEnv+"="+proxyExport)
	} else {
		dropHTTPProxies[ProxyURLEnv] = true
	}

	return setEnvKeyValues(env, dropHTTPProxies, overrides), cleanup, nil
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
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			goBin, goroot, err := Ensure(ctx, os.Getenv("BASHY_GO_VERSION"))
			if err != nil {
				return err
			}
			c := binmgr.Command(ctx, goBin, args...)
			c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
			if artifact.IsSealed("") {
				sealedEnv, cleanup, err := SealedCommandEnv(ctx, os.Environ(), goroot, "")
				if err != nil {
					return err
				}
				defer cleanup()
				c.Env = sealedEnv
			} else {
				c.Env = goCommandEnv(goroot)
			}
			return c.Run()
		},
	}
	return cmd
}

// PinGOROOT returns env with a single GOROOT entry pinned to goroot: an
// inherited GOROOT naming a different toolchain is replaced, never left as a
// duplicate the child may resolve first (todo 54a667814e3c). An empty goroot
// leaves env unchanged.
func PinGOROOT(env []string, goroot string) []string {
	if goroot == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GOROOT=") {
			out = append(out, kv)
		}
	}
	return append(out, "GOROOT="+goroot)
}

func goCommandEnv(goroot string) []string {
	env := append([]string{}, os.Environ()...)
	if runtime.GOOS == "windows" {
		env = normalizeWindowsTempEnv(env)
	}
	env = PinGOROOT(env, goroot)
	return notrack.Env(env)
}

// ChildEnv returns a copy of env with Go and generic tracking disabled.
// Exported for tests and for callers that build a child env directly.
func ChildEnv(env []string) []string { return notrack.Env(env) }

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
