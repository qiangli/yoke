package node

import (
	"reflect"
	"testing"

	"mvdan.cc/sh/v3/pathconv"
)

// Sprint 318: on Windows npm.cmd runs under `cmd.exe /d /c`. The drive-path
// adaptation binmgr applies to managed tools must not touch cmd.exe's own
// switches: with C: and D: present (a GitHub Windows runner) `/d` and `/c`
// became `D:\` and `C:\`, cmd.exe started interactively and typescript never
// landed (bashsharp/tour check --prepare, v0.30.0-dev).
func TestNpmArgvKeepsCmdSwitches(t *testing.T) {
	drives := pathconv.DrivesOf("CD")
	got := npmArgv("windows", drives, `C:\n\npm.cmd`, []string{"install", "--prefix", "/c/Users/x/ts", "typescript@5.9.3"})
	want := []string{"cmd.exe", "/d", "/c", `C:\n\npm.cmd`, "install", "--prefix", `C:\Users\x\ts`, "typescript@5.9.3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("npmArgv = %q\nwant      %q", got, want)
	}
	// The old composition, for the record: adapting the whole cmd.exe argv.
	if old := pathconv.NativeArgsIn(drives, []string{"cmd.exe", "/d", "/c"}); reflect.DeepEqual(old, []string{"cmd.exe", "/d", "/c"}) {
		t.Fatalf("expected the whole-argv adaptation to rewrite cmd.exe switches, got %q", old)
	}
	if got := npmArgv("linux", drives, "/n/npm", []string{"install"}); !reflect.DeepEqual(got, []string{"/n/npm", "install"}) {
		t.Fatalf("linux npmArgv = %q", got)
	}
}
