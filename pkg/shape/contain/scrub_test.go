package contain

import (
	"strings"
	"testing"
)

func TestScrubBasePerOS(t *testing.T) {
	parent := map[string]string{"SystemRoot": `D:\Win`, "SHAPE_TEST_SECRET": "hunter2", "USERPROFILE": `C:\Users\alice`}
	getenv := func(k string) string { return parent[k] }

	posix := scrubBase("linux", "/clone", getenv)
	if len(posix) != 3 || posix["PATH"] != "/usr/bin:/bin" || posix["HOME"] != "/clone" || posix["TMPDIR"] != "/clone" {
		t.Fatalf("posix base = %v", posix)
	}

	win := scrubBase("windows", `C:\clone`, getenv)
	want := map[string]string{
		"SystemRoot":   `D:\Win`,
		"PATH":         `D:\Win\System32;D:\Win`,
		"USERPROFILE":  `C:\clone`,
		"APPDATA":      `C:\clone\AppData\Roaming`,
		"LOCALAPPDATA": `C:\clone\AppData\Local`,
		"TEMP":         `C:\clone`,
		"TMP":          `C:\clone`,
		"HOME":         `C:\clone`,
		"TMPDIR":       `C:\clone`,
	}
	for k, v := range want {
		if win[k] != v {
			t.Errorf("windows %s = %q, want %q", k, win[k], v)
		}
	}
	for k, v := range win {
		if strings.Contains(v, "hunter2") || strings.Contains(v, "alice") || k == "SHAPE_TEST_SECRET" {
			t.Errorf("windows base leaked %s=%s", k, v)
		}
	}
	if got := scrubBase("windows", `C:\clone`, func(string) string { return "" })["SystemRoot"]; got != `C:\Windows` {
		t.Errorf("SystemRoot fallback = %q", got)
	}
}
