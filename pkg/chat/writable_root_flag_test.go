package chat

import (
	"bytes"
	"strings"
	"testing"
)

// Sprint: #413; Story: #1829; Story-ID: ce5985c75042
// chat and delegate accepted --sandbox but had no way to pass an explicit
// writable-root grant, so a coordination-dependent agent launched through them
// could not be granted its state. Each repeated grant must reach the argv.
func TestChatCLIForwardsRepeatedWritableRoots(t *testing.T) {
	isolatedRoom(t)
	cmd := NewChatCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"--agent", "codex", "-m", "hi", "--cwd", t.TempDir(), "--dry-run",
		"--sandbox", "workspace-write", "--writable-root", "/state with spaces", "--writable-root", "/repo/.git"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %s", err, errb.String())
	}
	got := out.String() + errb.String()
	for _, want := range []string{"--add-dir", "/state with spaces", "/repo/.git", "workspace-write"} {
		if !strings.Contains(got, want) {
			t.Fatalf("dry-run lost %q:\n%s", want, got)
		}
	}
}
