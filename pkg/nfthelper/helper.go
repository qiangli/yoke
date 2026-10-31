// Package nfthelper provides a private nft entry point for managed Podman.
// Importing it does not register nft as a Bashy command; only a symlink named
// nft with EnvName set routes here. The caller adds Dir to Podman's helper
// search path and PATH, leaving the user's shell lookup untouched.
package nfthelper

import (
	"fmt"
	"os"
	"path/filepath"

	nftcmd "github.com/qiangli/yoke/cmds/nft"
)

const EnvName = "BASHY_PODMAN_NFT_HELPER"

func init() {
	if os.Getenv(EnvName) == "1" && filepath.Base(os.Args[0]) == "nft" {
		os.Exit(nftcmd.RunHelper(os.Args[1:]))
	}
}

// Dir is outside the user's PATH and contains only the managed nft link.
func Dir() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return filepath.Join(cache, "bashy", "podman-conf", "nft-helper")
}

// Ensure points a private nft symlink at the running Bashy executable.
// Repointing it after a release makes the helper follow the current build.
func Ensure() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	link := filepath.Join(dir, "nft")
	if target, err := os.Readlink(link); err == nil && target == self {
		return dir, nil
	}
	tmp := fmt.Sprintf("%s.%d", link, os.Getpid())
	_ = os.Remove(tmp)
	if err := os.Symlink(self, tmp); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("replace nft helper: %w", err)
	}
	return dir, nil
}
