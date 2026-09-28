package winhelper

import "testing"

// Sprint 320: podman on Windows drives its WSL machine through wsl.exe, which
// lives in %SystemRoot%\System32. A caller whose PATH is only bashy's own
// directory (the Bash# tour's stripped leg, the release QA lane) got
// `exec: "wsl": executable file not found in %PATH%` from machine start/stop/
// list, so a text fence could never start the machine.
func TestWithSystemDir(t *testing.T) {
	for _, tc := range []struct{ path, root, want string }{
		{`C:\qa\bin`, `C:\Windows`, `C:\qa\bin;C:\Windows\System32`},
		{`C:\qa\bin;c:\windows\system32`, `C:\Windows`, `C:\qa\bin;c:\windows\system32`},
		{``, `C:\Windows`, `C:\Windows\System32`},
		{`C:\qa\bin`, ``, `C:\qa\bin`},
	} {
		if got := withSystemDir(tc.path, tc.root); got != tc.want {
			t.Errorf("withSystemDir(%q, %q) = %q, want %q", tc.path, tc.root, got, tc.want)
		}
	}
}
