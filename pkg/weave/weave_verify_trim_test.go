package weave

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A VERDICT MUST NOT DISCARD THE EVIDENCE FOR ITSELF.
//
// The old trim kept the last 2000 bytes. For `go test ./...` across 200+
// packages that is exactly the wrong end: the run finishes with the
// alphabetically last packages passing, so the tail is `ok` lines plus a bare
// `FAIL`, while every `FAIL <pkg>` and `# <pkg>` build error is in the head
// that got thrown away.
//
// The stored output then reads as a failure with no cause, and the summary line
// quotes a fragment of whatever `ok` line the byte cut landed inside — the
// literal observed verdict was "suite-gate-failed — dge 2.817s".
func TestVerifyTrimKeepsFailuresNotJustTheTail(t *testing.T) {
	var b strings.Builder
	b.WriteString("# github.com/qiangli/yoke/pkg/herald\n")
	b.WriteString("FAIL\tgithub.com/qiangli/yoke/pkg/herald\t0.5s\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "ok  \tgithub.com/qiangli/coreutils/pkg/filler%03d\t1.234s\n", i)
	}
	b.WriteString("FAIL\n")
	full := b.String()

	got := weaveTrimVerifyOutput(full, 2000)

	if len(got) > 2200 { // budget + the marker line
		t.Errorf("trimmed output is %d bytes, want ~2000", len(got))
	}
	if !strings.Contains(got, "FAIL\tgithub.com/qiangli/yoke/pkg/herald") {
		t.Error("the FAILING PACKAGE was discarded — this is the whole bug")
	}
	if !strings.Contains(got, "# github.com/qiangli/yoke/pkg/herald") {
		t.Error("the build-error line was discarded")
	}
}

// Short output is returned untouched — no marker noise on the common case.
func TestVerifyTrimLeavesShortOutputAlone(t *testing.T) {
	in := "ok  \tpkg/a\t1s\nok  \tpkg/b\t2s\n"
	if got := weaveTrimVerifyOutput(in, 2000); got != in {
		t.Errorf("short output was modified:\n%q", got)
	}
}

// A timeout marker is appended LAST, so a tail-only trim happened to keep it.
// Salient-first must keep it too — a timeout that looks like a plain failure is
// how a gate that never decided anything gets read as a verdict.
func TestVerifyTrimKeepsTheTimeoutMarker(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "ok  \tpkg/filler%03d\t1.234s\n", i)
	}
	b.WriteString("\n[weave: verify command timed out after 10m]")

	got := weaveTrimVerifyOutput(b.String(), 2000)
	if !strings.Contains(got, "timed out after 10m") {
		t.Error("the timeout marker was discarded; a timeout would read as a test failure")
	}
}

// When failures alone exceed the budget, keep the FIRST ones: the first failure
// is usually the cause and the rest are its consequences.
func TestVerifyTrimKeepsEarliestFailuresWhenOverBudget(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "FAIL\tgithub.com/qiangli/coreutils/pkg/failing%03d\t1.0s\n", i)
	}
	got := weaveTrimVerifyOutput(b.String(), 2000)
	if !strings.Contains(got, "failing000") {
		t.Error("the FIRST failure was dropped; it is the one most likely to be the cause")
	}
}

// The dirty-tree attestation is appended after weaveRunVerify has already
// selected salient output. A second tail-only budget enforcement here used to
// erase the leading failure line precisely when the tree was dirty.
func TestCollectVerifyEvidenceKeepsFailureWithDirtyLargeOutput(t *testing.T) {
	workspace := t.TempDir()
	gitT(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, workspace, "add", "tracked.txt")
	gitT(t, workspace, "-c", "user.email=test@example.com", "-c", "user.name=Test", "commit", "-qm", "seed")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, dirtyFiles, _ := weaveMeasureDirtiness(workspace)
	if !dirty || dirtyFiles != 1 {
		t.Fatalf("temporary worktree is not dirty as expected: dirty=%t files=%d", dirty, dirtyFiles)
	}

	command := "printf 'FAIL\\tpkg/regression\\t0.01s\\n'; " +
		"i=0; while [ $i -lt 500 ]; do printf 'ok filler%03d\\n' $i; i=$((i + 1)); done; exit 1"
	exit, output, tree := weaveCollectVerifyEvidence(workspace, "", command, nil, dirty, dirtyFiles)
	if exit == nil || *exit == 0 {
		t.Fatalf("verify exit=%v, want a failing exit", exit)
	}
	if tree != "working-tree-dirty" {
		t.Fatalf("verify tree=%q, want working-tree-dirty", tree)
	}
	if !strings.Contains(output, "FAIL\tpkg/regression\t0.01s") {
		t.Fatalf("salient FAIL line was discarded after dirty-tree attestation:\n%s", output)
	}
	if !strings.Contains(output, "VERIFY ATTESTED A DIRTY WORKING TREE") {
		t.Fatalf("dirty-tree attestation was discarded:\n%s", output)
	}
}
