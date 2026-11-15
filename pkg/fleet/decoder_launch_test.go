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
		// wantEnv carves out the one deliberate exception: Sprint 646 D5
		// pins Muse Contributor mode off on every run (documented vendor
		// env var, refused by no build), so muse's native launch carries
		// exactly that pair and nothing else. Anything beyond it — or any
		// env on another tool — is still a violation.
		wantEnv []string
	}{
		{"claude", []string{"claude", "--dangerously-skip-permissions", "-p", "hello"}, nil},
		{"agy", []string{"agy", "--dangerously-skip-permissions", "--print-timeout", "40m", "-p", "hello"}, nil},
		{"muse", []string{"muse", "exec", "--yolo", "hello"}, []string{"MUSE_CONTRIBUTOR_MODE=0"}},
		{"codex", []string{"codex", "exec", "--skip-git-repo-check", "--sandbox", "workspace-write", "hello"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, ok := c.Tool(tc.name)
			if !ok {
				t.Fatal("missing seed")
			}
			if got := tool.ArgvWithWorkspace("", "", "hello"); !reflect.DeepEqual(got, tc.argv) {
				t.Errorf("argv = %q, want %q", got, tc.argv)
			}
			if got := tool.LaunchEnv(LaunchVars{}); !reflect.DeepEqual(got, tc.wantEnv) {
				t.Errorf("native default env = %q, want %q", got, tc.wantEnv)
			}
		})
	}
}
