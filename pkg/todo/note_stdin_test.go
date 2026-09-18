package todo

import (
	"bytes"
	"strings"
	"testing"
)

// TestNoteStdinDash is the Sprint 180 gate: `--note -` reads the body from
// stdin on add and on edit, as `skill add -` does, instead of storing "-".
func TestNoteStdinDash(t *testing.T) {
	base := t.TempDir()
	run := func(stdin string, args ...string) string {
		t.Helper()
		cmd := NewTodoCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetIn(strings.NewReader(stdin))
		cmd.SetArgs(append([]string{"--base-dir", base}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	out := run("first body\nline two\n", "add", "read me", "--note", "-")
	id := strings.Fields(strings.TrimPrefix(out, "added "))[0]
	st := RepoStore(base)
	it, err := ResolveRef(st, id)
	if err != nil {
		t.Fatal(err)
	}
	// The store's markdown codec drops the trailing newline on round-trip.
	if it.Body != "first body\nline two" {
		t.Fatalf("add --note - stored %q", it.Body)
	}
	run("replaced\n", "edit", id, "--note", "-")
	if it, _ = ResolveRef(st, id); it.Body != "replaced" {
		t.Fatalf("edit --note - stored %q", it.Body)
	}
	// A literal note is untouched, and stdin is not consulted for it.
	run("never read", "edit", id, "--note", "literal")
	if it, _ = ResolveRef(st, id); it.Body != "literal" {
		t.Fatalf("edit --note literal stored %q", it.Body)
	}
}
