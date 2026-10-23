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
// command may lack a class, and no command may carry the path-only
// protected tier. ClassOf is total by construction; this pins that against
// the live atlas so a new table entry can never slip through unclassified.
func TestEveryAtlasCommandHasClass(t *testing.T) {
	vocab := map[string]bool{}
	for _, c := range src.Classes() {
		vocab[c] = true
	}
	if len(vocab) != 4 {
		t.Fatalf("Classes() = %q, want the 4-class vocabulary", src.Classes())
	}
	cmdVocab := map[string]bool{}
	for _, c := range src.CommandClasses() {
		cmdVocab[c] = true
	}
	if len(cmdVocab) != 3 {
		t.Fatalf("CommandClasses() = %q, want core|skill|external", src.CommandClasses())
	}
	cmds := src.AtlasCommands()
	if len(cmds) == 0 {
		t.Fatal("AtlasCommands() is empty — the ratchet would pass vacuously")
	}
	for _, cmd := range cmds {
		c := src.ClassOf(cmd)
		if !vocab[string(c)] {
			t.Errorf("ClassOf(%q) = %q, outside %q", cmd, c, src.Classes())
		}
		if c == src.ClassProtected {
			t.Errorf("ClassOf(%q) = protected: protection is per path, never per command", cmd)
		}
	}
	for _, v := range src.CoreVerbs() {
		if c := src.ClassOf(v); c != src.ClassCore {
			t.Errorf("ClassOf(%q) = %q, want core (locked verb set)", v, c)
		}
	}
}

// TestBashBuiltins61 pins the shell-owned builtin set: 61 names, with ".",
// ":", "[" and readonly present and time absent (time is a reserved word,
// not a builtin).
func TestBashBuiltins61(t *testing.T) {
	want := []string{
		".", ":", "[",
		"alias", "bg", "bind", "break",
		"builtin", "caller", "cd", "command",
		"compgen", "complete", "compopt", "continue",
		"declare", "dirs", "disown", "echo",
		"enable", "eval", "exec", "exit",
		"export", "false", "fc", "fg",
		"getopts", "hash", "help", "history",
		"jobs", "kill", "let", "local",
		"logout", "mapfile", "popd", "printf",
		"pushd", "pwd", "read", "readarray",
		"readonly", "return", "set", "shift",
		"shopt", "source", "suspend", "test",
		"times", "trap", "true", "type",
		"typeset", "ulimit", "umask", "unalias",
		"unset", "wait",
	}
	if len(want) != 61 {
		t.Fatalf("test lists %d names, want all 61 written out", len(want))
	}
	if got := src.BuiltinNames(); !slices.Equal(got, want) {
		t.Errorf("BuiltinNames() = %q, want %q", got, want)
	}
	if src.IsBuiltin("time") {
		t.Error("IsBuiltin(time) = true: time is a reserved word, not a builtin")
	}
	for _, b := range []string{".", ":", "[", "readonly"} {
		if !src.IsBuiltin(b) {
			t.Errorf("IsBuiltin(%q) = false, want true", b)
		}
	}
}

// TestClassDerivation pins the derivation rules behind ClassOf: origin axis
// plus core verb list, skill default. Guard-rail paths never promote a
// command out of skill.
func TestClassDerivation(t *testing.T) {
	cases := []struct {
		cmd  string
		want src.Class
		why  string
	}{
		{"cd", src.ClassCore, "bash builtin"},
		{".", src.ClassCore, "POSIX special builtin"},
		{"[", src.ClassCore, "test alias builtin"},
		{"readonly", src.ClassCore, "bash builtin"},
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
		{"weave", src.ClassSkill, "protected paths do not promote commands"},
		{"sprint", src.ClassSkill, "protected paths do not promote commands"},
		{"steward", src.ClassSkill, "in-process skill surface"},
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
