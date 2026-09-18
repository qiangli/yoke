package weave

import (
	"os"
	"path/filepath"
	"testing"
)

// THE SUITE GATE MUST BE ABLE TO BUILD THE TREE IT IS GATING.
//
// weaveRunCandidateSuiteGate clones the candidate into a temp dir. A go.mod
// that says `replace mvdan.cc/sh/v3 => ../sh` resolves that path relative to
// the MODULE directory, so cloning straight into /tmp/weave-pull-gate-XXXX
// makes ../sh mean /tmp/sh — which does not exist. Every package then fails to
// load (measured: 40 copies of "replacement directory ../sh does not exist"),
// and the gate reports suite-gate-failed for a candidate that is green in its
// workspace, quoting a truncated tail that points at an `ok` line.
//
// It fails CLOSED, so nothing bad merges. It also says nothing true about any
// candidate, which is worse than it sounds: every merge becomes manual, and the
// obvious conclusion — "the gate produces false rejects" — is one step from
// routing around the gate entirely.
func TestSiblingReplacesResolveInTheGateCheckout(t *testing.T) {
	umbrella := t.TempDir()
	root := filepath.Join(umbrella, "coreutils")
	for _, d := range []string{"coreutils", "sh"} {
		if err := os.MkdirAll(filepath.Join(umbrella, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gomod := "module example.com/coreutils\n\ngo 1.26\n\nreplace mvdan.cc/sh/v3 => ../sh\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := weaveLinkSiblingReplaces(root, dst); err != nil {
		t.Fatalf("weaveLinkSiblingReplaces: %v", err)
	}

	// The clone lands at dst/<reponame>, so ../sh from THERE must resolve.
	clone := filepath.Join(dst, filepath.Base(root))
	if _, err := os.Stat(filepath.Join(clone, "..", "sh")); err != nil {
		t.Fatalf("../sh does not resolve from the gate checkout: %v", err)
	}
}

// It links the DECLARED replaces only. The umbrella has a dozen projects side
// by side; a gate that quietly exposed all of them would let a candidate pass
// on a dependency its go.mod never declared.
func TestGateLinksOnlyDeclaredSiblings(t *testing.T) {
	umbrella := t.TempDir()
	root := filepath.Join(umbrella, "coreutils")
	for _, d := range []string{"coreutils", "sh", "kg", "cloudbox"} {
		if err := os.MkdirAll(filepath.Join(umbrella, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gomod := "module example.com/coreutils\n\nreplace mvdan.cc/sh/v3 => ../sh\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := t.TempDir()
	if err := weaveLinkSiblingReplaces(root, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "sh")); err != nil {
		t.Error("declared sibling sh was not linked")
	}
	for _, undeclared := range []string{"kg", "cloudbox"} {
		if _, err := os.Lstat(filepath.Join(dst, undeclared)); err == nil {
			t.Errorf("undeclared sibling %q was linked into the gate checkout", undeclared)
		}
	}
}

// A sibling that is not checked out on this host is NOT an error. A repo may
// legitimately point at one; the gate's job is to report what the compiler says
// about that, not to refuse to run.
func TestGateToleratesAMissingSibling(t *testing.T) {
	umbrella := t.TempDir()
	root := filepath.Join(umbrella, "coreutils")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gomod := "module example.com/coreutils\n\nreplace example.com/nope => ../nope\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := weaveLinkSiblingReplaces(root, t.TempDir()); err != nil {
		t.Fatalf("a missing sibling must not fail the gate: %v", err)
	}
}
