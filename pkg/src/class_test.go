package src_test

import (
	"slices"
	"testing"

	"github.com/qiangli/yoke/pkg/src"
)

// TestCoreVerbSetLocked is the drift ratchet: the 14 core verbs are the
// Sprint 458 M0 record's closed list. Widening or narrowing it here without
// amending the record silently reclasses commands — fail instead.
func TestCoreVerbSetLocked(t *testing.T) {
	want := []string{
		"agent", "model", "tool", "skill",
		"self", "src", "git", "dag",
		"commands", "inspect", "secret", "ask",
		"claim", "resource",
	}
	if got := src.CoreVerbs(); !slices.Equal(got, want) {
		t.Errorf("CoreVerbs() = %q, want %q (the locked 14-verb record)", got, want)
	}
}

// TestEveryAtlasCommandHasClass is the coverage ratchet: no catalogued
// command may lack a class. ClassOf is total by construction; this pins
// that against the live atlas so a new table entry can never slip through
// unclassified.
func TestEveryAtlasCommandHasClass(t *testing.T) {
	vocab := map[string]bool{}
	for _, c := range src.Classes() {
		vocab[c] = true
	}
	if len(vocab) != 4 {
		t.Fatalf("Classes() = %q, want the 4-class vocabulary", src.Classes())
	}
	cmds := src.AtlasCommands()
	if len(cmds) == 0 {
		t.Fatal("AtlasCommands() is empty — the ratchet would pass vacuously")
	}
	for _, cmd := range cmds {
		if c := src.ClassOf(cmd); !vocab[string(c)] {
			t.Errorf("ClassOf(%q) = %q, outside %q", cmd, c, src.Classes())
		}
	}
	for _, v := range src.CoreVerbs() {
		if c := src.ClassOf(v); c != src.ClassCore {
			t.Errorf("ClassOf(%q) = %q, want core (locked verb set)", v, c)
		}
	}
}

// TestClassDerivation pins the derivation rules behind ClassOf: origin axis
// plus core verb list, protected manifest, skill default.
func TestClassDerivation(t *testing.T) {
	cases := []struct {
		cmd  string
		want src.Class
		why  string
	}{
		{"cd", src.ClassCore, "bash builtin"},
		{"ls", src.ClassCore, "POSIX-required + GNU name"},
		{"cat", src.ClassCore, "POSIX-required + GNU name"},
		{"awk", src.ClassCore, "POSIX-required classic tool"},
		{"m4", src.ClassCore, "POSIX-provider external"},
		{"localedef", src.ClassCore, "POSIX-provider external"},
		{"posix-providers", src.ClassCore, "the provider front itself"},
		{"dag", src.ClassCore, "locked core verb"},
		{"git", src.ClassCore, "locked core verb beats managed-external origin"},
		{"src", src.ClassCore, "locked core verb"},
		{"resource", src.ClassCore, "locked core verb"},
		{"kubectl", src.ClassExternal, "bin-managed external"},
		{"gh", src.ClassExternal, "bin-managed external"},
		{"go", src.ClassExternal, "toolchain provisioner"},
		{"weave", src.ClassProtected, "implemented under protected pkg/weave"},
		{"sprint", src.ClassProtected, "implemented under protected pkg/weave"},
		{"steward", src.ClassProtected, "implemented under protected pkg/steward"},
		{"kb", src.ClassSkill, "in-process skill surface"},
		{"chat", src.ClassSkill, "in-process skill surface"},
		{"resources", src.ClassSkill, "yoke tool outside the core set"},
	}
	for _, tc := range cases {
		if got := src.ClassOf(tc.cmd); got != tc.want {
			t.Errorf("ClassOf(%q) = %q, want %q (%s)", tc.cmd, got, tc.want, tc.why)
		}
	}
}
