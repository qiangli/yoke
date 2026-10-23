package src

import (
	"os"
	"runtime/debug"
	"strings"
)

// commitOverrideEnv is the documented test hook for the release commit: the
// running binary's VCS stamp is fixed at link time, so tests pin it through
// the environment instead of the toolchain.
const commitOverrideEnv = "BASHY_SRC_COMMIT"

// ReleaseCommit returns the running binary's release commit: the vcs.revision
// stamp in its build info. It is the commit `src fetch` pins and `src show`
// reports. $BASHY_SRC_COMMIT overrides it (tests and development builds);
// "" means no commit is known.
func ReleaseCommit() string {
	if v := strings.TrimSpace(os.Getenv(commitOverrideEnv)); v != "" {
		return v
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return ""
}
