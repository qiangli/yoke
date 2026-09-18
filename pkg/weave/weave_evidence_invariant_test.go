package weave

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCleanExitWithoutDiffEvidenceIsNoOp(t *testing.T) {
	ev := weaveTerminalEvidence{CommitsAhead: 0, Dirty: false, UntrackedFiles: 0}
	if got := weaveTerminalState(0, nil, "", ev); got != "no-op" {
		t.Fatalf("exit 0 + clean tree + zero commits = %q, want no-op", got)
	}
	if !isTerminalState("no-op") {
		t.Fatal("no-op must be terminal so waiters cannot mistake it for live work")
	}
}

func TestCommittedCleanExitStillSubmits(t *testing.T) {
	ev := weaveTerminalEvidence{CommitsAhead: 1, Head: "abc123"}
	if got := weaveTerminalState(0, nil, "", ev); got != "submitted" {
		t.Fatalf("exit 0 + commit evidence = %q, want submitted", got)
	}
}

// Ratchet the fleet-evidence invariant at the constructor boundary: every
// combination without commits must remain unable to construct a success state.
func TestNoSuccessStateCanBeConstructedWithoutCommitEvidence(t *testing.T) {
	for _, exitCode := range []int{0, 1, 143} {
		for _, runErr := range []error{nil, errors.New("wrapper error")} {
			for _, killedBy := range []string{"", "watchdog"} {
				for _, dirty := range []bool{false, true} {
					ev := weaveTerminalEvidence{Dirty: dirty}
					got := weaveTerminalState(exitCode, runErr, killedBy, ev)
					if weaveStateAssertsSuccess(got) {
						t.Errorf("weaveTerminalState(%d, err=%v, killed=%q, %+v) = %q: success constructed without commits",
							exitCode, runErr, killedBy, ev, got)
					}
				}
			}
		}
	}
}

func TestOutsideWorkspacePathFromLogIsSurfacedOnRun(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	inside := filepath.Join(workspace, "pkg", "weave", "state.go")
	outside := filepath.Join(filepath.Dir(workspace), "stale-session", "od.go")
	logTail := "editing " + inside + "\ncompleted unrelated work in " + outside + "\n"

	got := weaveOutsideWorkspacePaths(logTail, workspace)
	if want := []string{outside}; !reflect.DeepEqual(got, want) {
		t.Fatalf("outside paths = %q, want %q", got, want)
	}
	it := &weaveItem{ID: 7, State: "no-op", OutsideWorkspacePaths: got}
	warning := weaveOutsideWorkspaceWarning(it)
	if !strings.Contains(warning, "worker referenced paths outside its workspace: "+outside) {
		t.Fatalf("warning did not surface outside path: %q", warning)
	}
	encoded, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"outside_workspace_paths"`) || !strings.Contains(string(encoded), outside) {
		t.Fatalf("run JSON did not carry outside path: %s", encoded)
	}
}
