// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"slices"
	"testing"
	"time"
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

// TestDelegateIdleOutlastsOneModelTurn: live on the launcher one silent model
// turn took 4m38s; the old 5m idle default killed the next one (S526.B1a2).
func TestDelegateIdleOutlastsOneModelTurn(t *testing.T) {
	timeout, idle := BashyDelegate{}.delegateTimeouts()
	if idle < 15*time.Minute || timeout < idle {
		t.Fatalf("defaults timeout=%v idle=%v; idle must be >= 15m and <= timeout", timeout, idle)
	}
	if to, id := (BashyDelegate{Timeout: time.Minute, IdleTimeout: time.Second}).delegateTimeouts(); to != time.Minute || id != time.Second {
		t.Fatalf("explicit values not kept: %v %v", to, id)
	}
}
