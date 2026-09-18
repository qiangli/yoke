// Package whycmd implements why(1) for process and port activity inspection.
//
// Backed by Apache-2.0 pranshuparmar/witr v0.3.3 as a checksum-pinned managed
// external binary (pkg/binmgr), with no upstream Go dependency compiled into
// coreutils.
package whycmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// PinnedVersion is the single checksum-pinned witr release version.
	PinnedVersion = "v0.3.3"
	// UpstreamTool is the name of the upstream managed binary in binmgr cache.
	UpstreamTool = "witr"
)

var cmd = &tool.Tool{
	Name:     "why",
	Synopsis: "Explain process and port activity, backed by managed witr v0.3.3.",
	Usage: `why [options] [process-or-service-name]
  e.g.: why nginx
        why --port 8080
        why --pid 1234
        why --file /var/log/app.log
        why --container my-container
        why --json nginx
        why --tree nginx`,
}

func init() {
	cmd.Run = run
	tool.Register(cmd)
}

// Injected seams for unit testing and hermetic verification.
var (
	resolveBinFn = defaultResolveBin
	execCmdFn    = defaultExecCmd
)

var (
	goos   = runtime.GOOS
	goarch = runtime.GOARCH
)

func defaultResolveBin(ctx context.Context, rc *tool.RunContext) (string, error) {
	// 0. Fail closed BEFORE cache/network for unsupported platforms.
	plat := goos + "/" + goarch
	if _, ok := binmgr.PinnedSHA256(UpstreamTool, PinnedVersion, plat); !ok {
		return "", fmt.Errorf("unsupported platform %s: no pinned digest for %s@%s", plat, UpstreamTool, PinnedVersion)
	}

	// Reject version overrides/unpinned requests fail-closed.
	if err := validateVersionOverride(rc); err != nil {
		return "", err
	}

	// 1. Fast cache check: if cached binary is present, skip GitHub API resolution.
	if root, err := binmgr.CacheDir(); err == nil {
		cached := filepath.Join(root, UpstreamTool, PinnedVersion, binmgr.BinaryName(UpstreamTool))
		if isExec(cached) {
			return cached, nil
		}
	}

	// 2. Resolve release metadata from GitHub and ensure binary with sha256 pin.
	spec := buildSpec(goos, goarch)

	toolObj, err := binmgr.ResolveGitHub(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("resolve witr: %w", err)
	}

	path, err := binmgr.Ensure(ctx, toolObj)
	if err != nil {
		return "", fmt.Errorf("ensure witr: %w", err)
	}
	return path, nil
}

func validateVersionOverride(rc *tool.RunContext) error {
	var reqVer string
	for _, envKey := range []string{"WHY_VERSION", "WITR_VERSION"} {
		if v := strings.TrimSpace(rc.Getenv(envKey)); v != "" {
			reqVer = v
			break
		}
	}
	if reqVer != "" {
		norm := reqVer
		if !strings.HasPrefix(norm, "v") {
			norm = "v" + norm
		}
		if norm != PinnedVersion {
			return fmt.Errorf("unsupported version override %q: only %s is pinned and supported", reqVer, PinnedVersion)
		}
	}
	return nil
}

func buildSpec(goos, goarch string) binmgr.GitHubSpec {
	spec := binmgr.GitHubSpec{
		Name:    UpstreamTool,
		Repo:    "pranshuparmar/witr",
		Version: PinnedVersion,
		AssetMatch: func(assetName, matchOS, matchArch string) bool {
			expected := fmt.Sprintf("witr-%s-%s", matchOS, matchArch)
			if matchOS == "windows" {
				expected += ".zip"
			}
			return assetName == expected
		},
	}
	if goos == "windows" {
		spec.Member = "witr.exe"
	}
	return spec
}

func defaultExecCmd(ctx context.Context, binPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, int, error) {
	c := exec.CommandContext(ctx, binPath, args...)
	c.Dir = dir
	if env != nil {
		c.Env = env
	} else {
		c.Env = []string{} // Explicit empty environment
	}
	c.Stdin = stdin
	c.Stdout = stdout
	c.Stderr = stderr

	if err := c.Start(); err != nil {
		return 126, 0, fmt.Errorf("start %s: %w", binPath, err)
	}

	if err := c.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if code := exitErr.ExitCode(); code >= 0 {
				return code, 0, nil
			}
			// Killed by signal
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				sig := int(ws.Signal())
				return 128 + sig, sig, nil
			}
		}
		return 1, 0, err
	}
	return 0, 0, nil
}

func isExec(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return fi.Mode()&0o111 != 0
}

func errPrintf(rc *tool.RunContext, format string, a ...any) {
	if rc != nil && rc.Err != nil {
		fmt.Fprintf(rc.Err, format, a...)
	}
}

func run(rc *tool.RunContext, args []string) int {
	rc.ExitSignal = 0
	ctx := rc.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	binPath, err := resolveBinFn(ctx, rc)
	if err != nil {
		errPrintf(rc, "why: %v\n", err)
		return 1
	}

	// Environment preservation: nil means explicit empty environment []string{}
	var cmdEnv []string
	if rc.Env != nil {
		cmdEnv = rc.Env
	} else {
		cmdEnv = []string{}
	}

	exitCode, sig, err := execCmdFn(ctx, binPath, args, rc.Dir, cmdEnv, rc.In, rc.Out, rc.Err)
	if sig > 0 {
		rc.ExitSignal = sig
	}
	if err != nil {
		errPrintf(rc, "why: %v\n", err)
		return exitCode
	}
	return exitCode
}
