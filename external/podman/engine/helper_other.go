//go:build !windows

package engine

func ensurePlatformHelperBinaries(cacheDir string) error {
	return nil
}
