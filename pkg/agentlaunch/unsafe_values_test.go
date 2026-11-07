package agentlaunch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
)

// zcode's yolo is a flag WITH A VALUE. Neither token alone is a kill-switch, so a
// single-token table let `--mode yolo` through the guard and through ReadOnly.
func TestValueQualifiedUnsafeFlags(t *testing.T) {
	t.Setenv(UnsafeLaunchEnv, "")
	for _, argv := range [][]string{
		{"--mode", "yolo", "-p", "hi"},
		{"--mode=yolo", "-p", "hi"},
		{"--sandbox=danger-full-access", "hi"},
	} {
		err := GuardUnsafeArgsFor("zcode", argv, Options{Containerized: func() bool { return false }})
		if err == nil {
			t.Fatalf("guard let %v through on an uncontained host", argv)
		}
		if !strings.Contains(err.Error(), "yolo") && !strings.Contains(err.Error(), "danger-full-access") {
			t.Fatalf("refusal should name the flag, got: %v", err)
		}
	}

	// A safe value of the same flag is not refused.
	if err := GuardUnsafeArgsFor("zcode", []string{"--mode", "plan", "-p", "hi"}, Options{Containerized: func() bool { return false }}); err != nil {
		t.Fatalf("--mode plan is not unsafe: %v", err)
	}

	want := []string{"-p", "hi"}
	if got := StripKillSwitches("zcode", []string{"--mode", "yolo", "-p", "hi"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("StripKillSwitches = %v, want %v", got, want)
	}
	if got := ReadOnlyArgs("zcode", []string{"--mode=yolo", "-p", "hi"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadOnlyArgs = %v, want %v", got, want)
	}
	// normalizeUnsafeFlags keeps a tool's value-qualified spelling intact.
	if got := normalizeUnsafeFlags("zcode", []string{"--mode", "yolo", "-p", "hi"}); !reflect.DeepEqual(got, []string{"--mode", "yolo", "-p", "hi"}) {
		t.Fatalf("normalizeUnsafeFlags = %v", got)
	}
}

// muse's canonical kill-switch is --yolo; another tool's spelling is dropped.
func TestMuseCanonicalUnsafeFlag(t *testing.T) {
	got := normalizeUnsafeFlags("muse", []string{"exec", "--dangerously-skip-permissions", "--yolo", "--yolo", "p"})
	if want := []string{"exec", "--yolo", "p"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeUnsafeFlags(muse) = %v, want %v", got, want)
	}
}

// A tool's declared unsafe spelling ADDS to the guard: a custom tool's own
// value-qualified kill-switch is refused uncontained, and its canonical flag
// wins normalization over the core fallback table.
func TestDeclaredUnsafeSpecAddsToGuard(t *testing.T) {
	t.Setenv(UnsafeLaunchEnv, "")
	spec := fleet.ToolUnsafe{Canonical: "--trust-all", Values: map[string][]string{"--approval": {"never"}}}
	opt := Options{Containerized: func() bool { return false }, Unsafe: spec}
	if err := GuardUnsafeArgsFor("custom", []string{"--approval", "never", "p"}, opt); err == nil {
		t.Fatal("declared value-qualified kill-switch passed the guard")
	}
	if err := GuardUnsafeArgsFor("custom", []string{"--approval", "ask", "p"}, opt); err != nil {
		t.Fatalf("safe value refused: %v", err)
	}
	if got := readOnlyArgsWith(spec, "custom", []string{"--approval=never", "p"}); !reflect.DeepEqual(got, []string{"p"}) {
		t.Fatalf("readOnlyArgsWith = %v", got)
	}
	if got := normalizeUnsafeFlagsWith(fleet.ToolUnsafe{Canonical: "--yolo"}, "claude", []string{"--dangerously-skip-permissions", "--yolo", "p"}); !reflect.DeepEqual(got, []string{"--yolo", "p"}) {
		t.Fatalf("declared canonical did not win: %v", got)
	}
}
