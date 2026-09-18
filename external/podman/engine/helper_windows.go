//go:build windows

package engine

import (
	"github.com/qiangli/yoke/external/podman/winhelper"
)

// ensurePlatformHelperBinaries stages the Windows helper binaries
// (gvproxy, win-sshproxy) into cacheDir.
//
// The implementation lives in the light external/podman/winhelper
// package rather than here, because bashy's SHIPPED Windows build does
// not link this engine (the in-process libpod engine is behind
// `-tags bashy_engines`) and still needs the same provisioning before it
// execs a podman binary. Keeping one copy keeps the pinned release +
// digests from drifting between the linked and exec'd paths.
func ensurePlatformHelperBinaries(cacheDir string) error {
	return winhelper.Ensure(cacheDir)
}
