package fleet

import (
	"reflect"
	"testing"
)

// New decoder metadata must not affect the native, unbound CLI launch.
func TestDecoderSeedsPreserveNativeDefaultLaunch(t *testing.T) {
	c := baseline(t)
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "-p", "hello"}},
		{"agy", []string{"agy", "--dangerously-skip-permissions", "--print-timeout", "40m", "-p", "hello"}},
		{"muse", []string{"muse", "exec", "--yolo", "hello"}},
		{"codex", []string{"codex", "exec", "--skip-git-repo-check", "--sandbox", "workspace-write", "hello"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := c.Tool(tc.name)
			if !ok {
				t.Fatal("missing seed")
			}
			if got := tool.ArgvWithWorkspace("", "", "hello"); !reflect.DeepEqual(got, tc.argv) {
				t.Errorf("argv = %q, want %q", got, tc.argv)
			}
			if got := tool.LaunchEnv(LaunchVars{}); len(got) != 0 {
				t.Errorf("native default env = %q, want none", got)
			}
		})
	}
}
