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
)

// DefaultVersion is the PowerShell 7.6 LTS runtime used by both fences.
const DefaultVersion = "7.6.6"

var releaseBase = "https://github.com/PowerShell/PowerShell/releases/download"

type releaseAsset struct {
	filename string
	sha256   string
}

// releaseAssets is copied from the upstream v7.6.6 hashes.sha256 release
// asset. Keys distinguish musl because GOOS/GOARCH alone cannot.
var releaseAssets = map[string]releaseAsset{
	"7.6.6/windows/amd64":    {"PowerShell-7.6.6-win-x64.zip", "02fe458be20493fbdf43f61ea20610b811ee6c738ab1676c61b9cfcd1a33c860"},
	"7.6.6/windows/arm64":    {"PowerShell-7.6.6-win-arm64.zip", "bbde9dda31d148415eccb5fbe1638e6400a144187b006e5b3fd8ec2f39d781be"},
	"7.6.6/linux/amd64":      {"powershell-7.6.6-linux-x64.tar.gz", "ddbc4a2d113bbd46d283cfedcbcd117a70caefd7673f41f2b4e0000badf103bc"},
	"7.6.6/linux/arm64":      {"powershell-7.6.6-linux-arm64.tar.gz", "924829e54c983648f6f1419a2dc7f9433c861b2fb5bd57736ff096c24f133729"},
	"7.6.6/linux/amd64/musl": {"powershell-7.6.6-linux-musl-x64.tar.gz", "9537c256a60c34f6bc2dd60c1c10b31a0c2ef26e96799d066be78325ab4947cc"},
	"7.6.6/darwin/amd64":     {"powershell-7.6.6-osx-x64.tar.gz", "e325ed9f666894eb39a5ea52800b602da2fb4242bbe9747ceddb39cdc66de805"},
	"7.6.6/darwin/arm64":     {"powershell-7.6.6-osx-arm64.tar.gz", "6df833d094ebac1c1a74340d7b3437f4aaf5e03ce640484a1c4359f3ce8b3db1"},
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

// Ensure returns the pinned managed pwsh executable. A leading v in version is
// tolerated; an empty version selects DefaultVersion. Only committed archive
// pins are accepted, so an arbitrary override cannot turn download + exec into
// trust-on-first-use.
func Ensure(ctx context.Context, version string) (string, error) {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		version = DefaultVersion
	}
	musl := runtime.GOOS == "linux" && !hasGlibc()
	asset, err := assetFor(version, runtime.GOOS, runtime.GOARCH, musl)
	if err != nil {
		return "", err
	}
	return ensureAsset(ctx, version, asset)
}

func ensureAsset(ctx context.Context, version string, asset releaseAsset) (string, error) {
	entry := entrypoint(runtime.GOOS)
	tool := binmgr.Tool{
		Name: "pwsh", Version: version,
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
	bin, err := Ensure(ctx, version)
	if err != nil {
		return nil, err
	}
	return []string{bin, "-NoLogo", "-NoProfile", "-NonInteractive"}, nil
}

// ChildEnv returns a copy of env with PowerShell network telemetry and update
// checks disabled. On Linux it selects invariant globalization by default, so
// the pinned runtime also starts on minimal hosts and in the scratch image
// without ICU. Callers with ICU can explicitly set the .NET variable to 0.
// Existing PowerShell setting spellings are replaced rather than duplicated.
func ChildEnv(env []string) []string {
	out := make([]string, 0, len(env)+3)
	hasGlobalizationSetting := false
	for _, item := range env {
		name, _, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(name, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT") {
			hasGlobalizationSetting = true
		}
		if ok && (strings.EqualFold(name, "POWERSHELL_TELEMETRY_OPTOUT") || strings.EqualFold(name, "POWERSHELL_UPDATECHECK")) {
			continue
		}
		out = append(out, item)
	}
	out = append(out, "POWERSHELL_TELEMETRY_OPTOUT=1", "POWERSHELL_UPDATECHECK=Off")
	if runtime.GOOS == "linux" && !hasGlobalizationSetting {
		out = append(out, "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1")
	}
	return out
}

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
			bin, err := Ensure(cmd.Context(), os.Getenv("BASHY_PWSH_VERSION"))
			if err != nil {
				return err
			}
			argv := append([]string{"-NoLogo", "-NoProfile"}, args...)
			child := Command(cmd.Context(), bin, argv...)
			child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
			return child.Run()
		},
	}
}

var glibcLoaders = []string{
	"/lib64/ld-linux-x86-64.so.2",
	"/lib/ld-linux-aarch64.so.1",
	"/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2",
	"/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1",
}

func hasGlibc() bool {
	for _, path := range glibcLoaders {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}
