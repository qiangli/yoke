// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package pwsh provisions the official PowerShell runtime used by Bash#'s
// PowerShell and C# fences. The runtime is downloaded, digest-verified and
// executed as a separate process; it is never linked into or bundled with
// bashy.
package pwsh

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/muslrt"
)

// DefaultVersion is the PowerShell 7.6 LTS runtime used by both fences.
const DefaultVersion = "7.6.6"

var releaseBase = "https://github.com/PowerShell/PowerShell/releases/download"

type releaseAsset struct {
	filename string
	sha256   string
	// fxdependent marks the framework-dependent IL archive, which runs on a
	// separately pinned .NET runtime (musl.go) instead of carrying its own.
	fxdependent bool
}

// releaseAssets is copied from the upstream v7.6.6 hashes.sha256 release
// asset. Keys distinguish musl because GOOS/GOARCH alone cannot. Upstream
// publishes no self-contained musl arm64 archive; that host gets the
// architecture-neutral framework-dependent archive (IL only, no ReadyToRun) on
// the pinned musl arm64 .NET runtime.
var releaseAssets = map[string]releaseAsset{
	"7.6.6/windows/amd64":    {"PowerShell-7.6.6-win-x64.zip", "02fe458be20493fbdf43f61ea20610b811ee6c738ab1676c61b9cfcd1a33c860", false},
	"7.6.6/windows/arm64":    {"PowerShell-7.6.6-win-arm64.zip", "bbde9dda31d148415eccb5fbe1638e6400a144187b006e5b3fd8ec2f39d781be", false},
	"7.6.6/linux/amd64":      {"powershell-7.6.6-linux-x64.tar.gz", "ddbc4a2d113bbd46d283cfedcbcd117a70caefd7673f41f2b4e0000badf103bc", false},
	"7.6.6/linux/arm64":      {"powershell-7.6.6-linux-arm64.tar.gz", "924829e54c983648f6f1419a2dc7f9433c861b2fb5bd57736ff096c24f133729", false},
	"7.6.6/linux/amd64/musl": {"powershell-7.6.6-linux-musl-x64.tar.gz", "9537c256a60c34f6bc2dd60c1c10b31a0c2ef26e96799d066be78325ab4947cc", false},
	"7.6.6/linux/arm64/musl": {"powershell-7.6.6-linux-x64-musl-noopt-fxdependent.tar.gz", "29a3d89b5d54f3aa67decaf64bd9cbf72cea469aa9e69330e5dc2c5ffdb37f38", true},
	"7.6.6/darwin/amd64":     {"powershell-7.6.6-osx-x64.tar.gz", "e325ed9f666894eb39a5ea52800b602da2fb4242bbe9747ceddb39cdc66de805", false},
	"7.6.6/darwin/arm64":     {"powershell-7.6.6-osx-arm64.tar.gz", "6df833d094ebac1c1a74340d7b3437f4aaf5e03ce640484a1c4359f3ce8b3db1", false},
}

func platformKey(version, goos, goarch string, musl bool) string {
	key := version + "/" + goos + "/" + goarch
	if goos == "linux" && musl {
		key += "/musl"
	}
	return key
}

func assetFor(version, goos, goarch string, musl bool) (releaseAsset, error) {
	key := platformKey(version, goos, goarch, musl)
	asset, ok := releaseAssets[key]
	if !ok || strings.TrimSpace(asset.sha256) == "" {
		return releaseAsset{}, fmt.Errorf("pwsh: no pinned PowerShell %s asset and sha256 for %s/%s%s; supported pin is %s",
			version, goos, goarch, muslSuffix(goos, musl), DefaultVersion)
	}
	return asset, nil
}

func muslSuffix(goos string, musl bool) string {
	if goos == "linux" && musl {
		return " (musl)"
	}
	return ""
}

func entrypoint(goos string) string {
	if goos == "windows" {
		return "pwsh.exe"
	}
	return "pwsh"
}

// Launch is how to start the pinned PowerShell on this host: the command
// prefix and the environment it needs beyond the caller's. On glibc Linux,
// macOS and Windows it is the pwsh executable alone. On Linux without glibc
// (the FROM-scratch bashy image) it may be the .NET muxer plus pwsh.dll, with
// LD_LIBRARY_PATH naming the private runtime libraries and invariant
// globalization (musl.go).
type Launch struct {
	Argv []string
	Env  []string // KEY=VALUE; LD_LIBRARY_PATH is prepended to the caller's
}

// Resolve provisions the pinned PowerShell for this host and returns how to
// start it. A leading v in version is tolerated; an empty version selects
// DefaultVersion. Only committed archive pins are accepted, so an arbitrary
// override cannot turn download + exec into trust-on-first-use.
func Resolve(ctx context.Context, version string) (Launch, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		version = DefaultVersion
	}
	musl := runtime.GOOS == "linux" && !hasGlibc()
	asset, err := assetFor(version, runtime.GOOS, runtime.GOARCH, musl)
	if err != nil {
		return Launch{}, err
	}
	if musl {
		return resolveMusl(ctx, version, asset)
	}
	bin, err := ensureAsset(ctx, version, asset)
	if err != nil {
		return Launch{}, err
	}
	return Launch{Argv: []string{bin}}, nil
}

// Ensure returns the pinned managed pwsh executable. It fails on a host whose
// PowerShell starts through the .NET muxer (Linux arm64 without glibc); use
// Resolve there.
func Ensure(ctx context.Context, version string) (string, error) {
	launch, err := Resolve(ctx, version)
	if err != nil {
		return "", err
	}
	if len(launch.Argv) != 1 {
		return "", fmt.Errorf("pwsh: PowerShell on this host starts as %q; use pwsh.Resolve", launch.Argv)
	}
	return launch.Argv[0], nil
}

func ensureAsset(ctx context.Context, version string, asset releaseAsset) (string, error) {
	entry := entrypoint(runtime.GOOS)
	name := "pwsh"
	if asset.fxdependent {
		// Its own cache name: the archive's pwsh apphost is x64 and must
		// never be found as this host's pwsh by a cache lookup.
		name = "pwsh-fxdependent"
	}
	tool := binmgr.Tool{
		Name: name, Version: version,
		Assets: map[string]binmgr.Asset{
			binmgr.Platform(): {
				URL:        releaseBase + "/v" + version + "/" + asset.filename,
				SHA256:     asset.sha256,
				Tree:       true,
				Entrypoint: entry,
			},
		},
	}
	return binmgr.Ensure(ctx, tool)
}

// FenceArgv returns the managed command prefix shared by powershell and csharp
// fences. Profiles, banners and interactive prompts are disabled so worker
// behavior does not depend on host configuration.
func FenceArgv(ctx context.Context, version string) ([]string, error) {
	launch, err := Resolve(ctx, version)
	if err != nil {
		return nil, err
	}
	return launch.FenceArgv(), nil
}

// FenceArgv is the fence command prefix for an already resolved launch.
func (l Launch) FenceArgv() []string {
	return append(append([]string(nil), l.Argv...), "-NoLogo", "-NoProfile", "-NonInteractive")
}

// Overrides returns the KEY=VALUE settings the PowerShell child needs on top
// of base: telemetry and update checks off, plus the launch's own variables.
// LD_LIBRARY_PATH is prepended to base's value rather than replacing it.
func (l Launch) Overrides(base []string) []string {
	out := []string{"POWERSHELL_TELEMETRY_OPTOUT=1", "POWERSHELL_UPDATECHECK=Off"}
	for _, kv := range l.Env {
		name, value, _ := strings.Cut(kv, "=")
		if name == "LD_LIBRARY_PATH" {
			if prev := lookup(base, name); prev != "" && prev != value && !strings.HasPrefix(prev, value+":") {
				kv = name + "=" + value + ":" + prev
			} else if prev != "" {
				kv = name + "=" + prev
			}
		}
		out = append(out, kv)
	}
	return out
}

// Environ is base with Overrides applied (existing spellings replaced).
func (l Launch) Environ(base []string) []string {
	over := l.Overrides(base)
	names := map[string]bool{}
	for _, kv := range over {
		name, _, _ := strings.Cut(kv, "=")
		names[strings.ToUpper(name)] = true
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if name, _, ok := strings.Cut(kv, "="); ok && names[strings.ToUpper(name)] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, over...)
}

func lookup(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
			return v
		}
	}
	return ""
}

// ChildEnv returns a copy of env with PowerShell network telemetry and update
// checks disabled. Existing spellings are replaced rather than duplicated.
func ChildEnv(env []string) []string { return Launch{}.Environ(env) }

// Command is the execution seam for the managed runtime. It deliberately goes
// through binmgr.Command so Windows receives the same path conversion as every
// other managed tool.
func Command(ctx context.Context, bin string, args ...string) *exec.Cmd {
	child := binmgr.Command(ctx, bin, args...)
	child.Env = ChildEnv(os.Environ())
	return child
}

// NewCmd is the optional `bashy pwsh` front door. It shares the fence cache and
// pin policy, disables profile loading, and leaves interactive mode available
// when the caller supplies no command.
func NewCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "pwsh",
		Short:              "Run pinned PowerShell 7.6 LTS, auto-provisioned and profile-free",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			launch, err := Resolve(cmd.Context(), os.Getenv("BASHY_PWSH_VERSION"))
			if err != nil {
				return err
			}
			argv := append(append(launch.Argv[1:len(launch.Argv):len(launch.Argv)], "-NoLogo", "-NoProfile"), args...)
			child := Command(cmd.Context(), launch.Argv[0], argv...)
			child.Env = launch.Environ(os.Environ())
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			return child.Run()
		},
	}
}

func hasGlibc() bool { return muslrt.HasGlibc() }
