package engine

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const managedPodmanVersion = "latest"

func ensureManagedPodman(ctx context.Context) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
		Name:       "podman-remote",
		Repo:       "containers/podman",
		Version:    managedPodmanVersion,
		Member:     "podman",
		AssetMatch: podmanAssetMatch,
	})
	if err != nil {
		return "", fmt.Errorf("resolve managed podman: %w", err)
	}
	if runtime.GOOS == "windows" {
		asset := tool.Assets[binmgr.Platform()]
		asset.Tree = true
		asset.Entrypoint = "podman-" + strings.TrimPrefix(tool.Version, "v") + "/usr/bin/podman.exe"
		tool.Assets[binmgr.Platform()] = asset
	}
	return binmgr.Ensure(ctx, tool)
}

func podmanAssetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.Contains(n, "podman-remote") {
		return false
	}
	switch goos {
	case "darwin":
		if !strings.Contains(n, "darwin") {
			return false
		}
	case "windows":
		if !strings.Contains(n, "windows") {
			return false
		}
	case "linux":
		if !strings.Contains(n, "linux") {
			return false
		}
	default:
		return false
	}
	if goarch == "amd64" {
		return strings.Contains(n, "amd64") || strings.Contains(n, "x86_64")
	}
	return strings.Contains(n, goarch)
}

func managedPodmanPlatform() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}
