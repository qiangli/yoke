// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"slices"
	"testing"
)

func TestShapedDelegateEnvAllowListPerOS(t *testing.T) {
	windowsOnly := []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "SystemRoot", "PATHEXT", "ComSpec"}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		names := delegateEnvNames(goos)
		for _, k := range []string{"PATH", "HOME", "TMPDIR", "BASHY_HOME"} {
			if !slices.Contains(names, k) {
				t.Errorf("%s: allow-list lacks %s", goos, k)
			}
		}
		for _, k := range windowsOnly {
			if got := slices.Contains(names, k); got != (goos == "windows") {
				t.Errorf("%s: %s allowed = %v", goos, k, got)
			}
		}
		for _, k := range names {
			if k == "" || slices.Contains([]string{"BASHY_SESSION_KEY", "GITHUB_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "USERDOMAIN"}, k) {
				t.Errorf("%s: unexpected variable %q in allow-list", goos, k)
			}
		}
	}
	if len(delegateEnvNames("windows")) != len(delegateEnvAllow)+len(windowsOnly) {
		t.Fatal("windows allow-list is not exactly the POSIX list plus the Windows profile/system variables")
	}
}
