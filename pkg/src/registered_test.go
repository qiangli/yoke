package src

import "testing"

// A registered command maps to the file the shell's registry names, as a
// skill-tier unit; without the hook it stays unknown rather than invented.
func TestSourceConsultsTheRegistry(t *testing.T) {
	old := RegisteredSource
	t.Cleanup(func() { RegisteredSource = old })

	RegisteredSource = nil
	if _, _, err := Source("record-test-cmd"); err == nil {
		t.Fatal("unregistered name resolved without a registry")
	}

	RegisteredSource = func(cmd string) (string, string, bool) {
		if cmd == "record-test-cmd" {
			return RegisteredRepo, "/src/examples/record/record.bsh", true
		}
		return "", "", false
	}
	info, err := Show("record-test-cmd")
	if err != nil {
		t.Fatal(err)
	}
	if info.Repo != RegisteredRepo || info.Package != "/src/examples/record/record.bsh" || info.Class != ClassSkill {
		t.Fatalf("Show = %+v", info)
	}
	// The registry never overrides what bashy ships.
	if repo, _, err := Source("cd"); err != nil || repo != "bashy" {
		t.Fatalf("builtin cd resolved to %q, %v", repo, err)
	}
}
