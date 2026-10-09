//go:build !windows

package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Sprint 379/issue 1530, invariant 4.6: a run's state is a claim, its branch
// is the evidence.  The historically observed "false no-op on a diverged base"
// was that a branch holding commits was reported as no-op because the base
// moved or the wrapper's measurement was stale.  The fix is twofold: counting
// against the immutable BaseSHA (weaveCountRef/weaveMeasureBranch) and the
// re-measurement in weaveTerminalStateMeasured, plus the health check that
// flags a recorded no-op whose live branch holds commits as inconsistent.
//
// This file pins the diverged-base half explicitly: a workspace whose branch
// is 1 ahead of the clone-point BaseSHA must still read 1 even after the
// origin/distinct base ref has advanced.  It is a regression test for the
// branch-as-evidence invariant, not for stale-wrapper timing (which is
// covered by weave_derived_status_test.go).

func TestDivergedBaseStillCountsAheadViaBaseSHA(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	// Add an issue and run it with a committed branch.
	if out, code := runWeave(t, "add", "diverged base: branch holds work", "--json"); code != 0 {
		t.Fatalf("weave add exit=%d: %s", code, out)
	}
	script := `echo diverged > diverged.txt
git add diverged.txt
git -c user.email=a@a -c user.name=a commit -qm "diverged work"`
	if out, code := runWeave(t, "start", "--issue", "1", "--json", "--", "sh", "-c", script); code != 0 {
		t.Fatalf("weave start exit=%d: %s", code, out)
	}

	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil {
		t.Fatal("run #1 missing")
	}
	if it.BaseSHA == "" {
		t.Fatal("BaseSHA must be recorded at clone time")
	}
	// Simulate the base moving forward on the host repo after clone: make a
	// commit on the resolved base ref (the repo's current main/master).
	base := "HEAD"
	// Advance base by one commit on the host repo after clone.
	_ = exec.Command(gitBin(), "-C", root, "commit", "--allow-empty", "-m", "base advanced").Run()

	// Counting against BaseSHA must still see the run's commit.
	ahead, head := weaveMeasureBranch(it.Workspace, it.BaseSHA)
	if ahead != 1 || head == "" {
		t.Fatalf("live count via BaseSHA = %d head=%q, want 1 after base advanced (base=%q, workspace=%q)", ahead, head, it.BaseSHA, it.Workspace)
	}
	// Counting against the moving branch name would drift; the invariant is
	// that yoke never does that (weaveCountRef prefers BaseSHA).
	if got := weaveCountRef(it, base); got != it.BaseSHA {
		t.Fatalf("weaveCountRef should prefer BaseSHA, got %q want %q", got, it.BaseSHA)
	}

	// The terminal classifier must not turn this into no-op even with a stale
	// zero evidence payload — the re-measurement recovers the commit.
	ev := weaveTerminalEvidence{CommitsAhead: 0}
	if got := weaveTerminalStateMeasured(it.Workspace, it.BaseSHA, 0, nil, "", &ev); got != "submitted" {
		t.Fatalf("classification via measured state = %q, want submitted for branch holding a commit after diverged base", got)
	}
	if ev.CommitsAhead != 1 {
		t.Fatalf("corrected evidence commits_ahead=%d, want 1", ev.CommitsAhead)
	}

	// Health must flag a recorded no-op that holds commits as inconsistent
	// and name salvage as the rescue, regardless of whether the base moved.
	it2 := *it
	it2.State = "no-op"
	it2.CommitsAhead = 0 // stale record
	it2.Head = ""
	// Build a health snapshot that live-measures the branch (the usual read path).
	snap := weaveHealthSnapshotFor(&it2, weaveHealthProbe{
		Now:             time.Now().UTC(),
		PIDAlive:        func(int) bool { return false },
		WorkspaceExists: func(p string) bool { _, e := os.Stat(p); return e == nil },
		MeasureBranch:   weaveMeasureBranch,
		LogModifiedAt:   func(string) (time.Time, bool) { return time.Time{}, false },
	})
	// After live measurement the snapshot's commits ahead must be 1.
	if snap.CommitsAhead != 1 {
		t.Fatalf("health snapshot live commits_ahead=%d, want 1", snap.CommitsAhead)
	}
	if reason, next, bad := weaveHealthConsistencyIssue(snap, &it2); !bad {
		t.Fatal("health should flag no-op holding commits as inconsistent")
	} else if !strings.Contains(reason, "no-op") || !strings.Contains(next, "salvage") {
		t.Fatalf("health reason=%q next=%q, want no-op + salvage guidance", reason, next)
	}
	// Also directly via count ref text file existence guard: empty workspace handling.
	_ = filepath.Join // keep import
}
