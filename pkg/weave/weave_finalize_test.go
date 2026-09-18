package weave

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWeaveRunConsumesCapacity(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  bool
	}{
		{name: "todo", state: "todo", want: true},
		{name: "allocated", state: "allocated", want: true},
		{name: "working", state: "working", want: true},
		{name: "finalizing", state: "finalizing", want: true},
		{name: "paused", state: "paused", want: false},
		{name: "submitted", state: "submitted", want: false},
		{name: "failed", state: "failed", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := weaveRunConsumesCapacity(tt.state); got != tt.want {
				t.Fatalf("weaveRunConsumesCapacity(%q) = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestWeaveWaitAllTreatsFinalizingAsInFlight(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:         1,
		Title:      "terminal evidence still pending",
		State:      "finalizing",
		Completion: "conductor-finalizing-observed-idle",
		Workspace:  root + "/workspace",
	}}}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "wait", "--all", "--timeout", "100ms", "--json")
	if code == 0 {
		t.Fatalf("wait --all returned success for a finalizing run:\n%s", out)
	}
	if !strings.Contains(out, "timeout after 100ms") {
		t.Fatalf("wait --all did not time out on a finalizing run:\n%s", out)
	}
}

// An interactive TUI can report Done and return to its prompt while its process
// remains open.  A conductor may explicitly attest that idle state, but weave
// must derive the terminal result from the workspace branch, never that prose.
func TestWeaveFinalizeObservedIdleSubmitsMeasuredCommit(t *testing.T) {
	root := setupIsolationFixture(t)
	base := strings.TrimSpace(gitT(t, root, "rev-parse", "HEAD"))
	workspace := t.TempDir() + "/workspace"
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	gitT(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	mustWrite(t, workspace+"/done.txt", "done\n")
	gitT(t, workspace, "add", "done.txt")
	gitT(t, workspace, "commit", "-qm", "agent complete")
	t.Chdir(root)
	// A real, live wrapper PID reproduces the failure shape: the interactive
	// process is still alive after its work commit. finalize must target only
	// this PID and leave a durable terminal record rather than false capacity.
	wrapper := exec.Command("sleep", "60")
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = wrapper.Process.Kill()
		_, _ = wrapper.Process.Wait()
	}()
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:         1,
		Title:      "interactive completion",
		State:      "working",
		Workspace:  workspace,
		Branch:     "agent/weave-issue-1",
		BaseSHA:    base,
		WrapperPid: wrapper.Process.Pid,
	}}}); err != nil {
		t.Fatal(err)
	}

	pause := filepath.Join(t.TempDir(), "finalize-claim")
	if err := os.WriteFile(pause, []byte("pause"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_TEST_FINALIZE_AFTER_CLAIM_FILE", pause)
	type result struct {
		out  string
		code int
	}
	finished := make(chan result, 1)
	go func() {
		out, code := runWeave(t, "finalize", "1", "--observed-idle", "--json")
		finished <- result{out, code}
	}()
	ready := pause + ".ready"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("finalize did not durably claim the live wrapper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !pidAlive(wrapper.Process.Pid) {
		t.Fatal("fixture wrapper died before the terminal-writer interleaving")
	}
	// This is the normal wrapper's terminal-write decision point. A live wrapper
	// attempts it after finalization has claimed the record; it must yield rather
	// than overwrite the measured completion with killed/failed state.
	writerYielded := false
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		if weaveWrapperTerminalClaimed(it) {
			writerYielded = true
			return nil
		}
		it.State = "killed" // would be the forced-stop terminal write
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !writerYielded {
		t.Fatal("normal terminal writer did not yield to finalization claim")
	}
	if err := os.Remove(pause); err != nil {
		t.Fatal(err)
	}
	r := <-finished
	out, code := r.out, r.code
	if code != 0 {
		t.Fatalf("finalize failed (exit %d): %s", code, out)
	}
	_ = wrapper.Wait() // SIGTERM is expected; returning proves the named wrapper is gone.
	if pidAlive(wrapper.Process.Pid) {
		t.Fatal("named live wrapper survived finalize")
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil || it.State != "submitted" || it.CommitsAhead != 1 || it.FinishedAt.IsZero() {
		t.Fatalf("finalize did not persist measured terminal evidence: %#v", it)
	}
	if it.Completion != "conductor-finalized-observed-idle" || it.ExitCode != nil || it.KilledBy != "" {
		t.Fatalf("finalize must not claim a tool exit or kill: %#v", it)
	}
}

func TestWeaveWrapperTerminalYieldsToFinalizeClaim(t *testing.T) {
	it := &weaveItem{State: "finalizing", Completion: "conductor-finalizing-observed-idle"}
	if !weaveWrapperTerminalClaimed(it) {
		t.Fatal("wrapper terminal writer must yield to an explicit finalization claim")
	}
	if weaveWrapperTerminalClaimed(&weaveItem{State: "working", Completion: it.Completion}) {
		t.Fatal("working item must not be mistaken for a finalization claim")
	}
}

func TestWeaveRecoverAbandonedFinalization(t *testing.T) {
	root := setupIsolationFixture(t)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{
		{
			ID:           1,
			State:        "finalizing",
			Completion:   "conductor-finalizing-observed-idle",
			FinalizerPID: 2147483647, // impossible: conductor died after claiming
			FinalizingAt: time.Now().UTC().Add(-weaveFinalizationLease - time.Second),
			Workspace:    root + "/preserve-for-salvage",
			WrapperPid:   0,
		},
		{
			ID:           2,
			State:        "finalizing",
			Completion:   "conductor-finalizing-observed-idle",
			FinalizerPID: 2147483647,
			WrapperPid:   os.Getpid(),                                                 // wrapper is still alive, so release it
			FinalizingAt: time.Now().UTC().Add(-weaveFinalizationLease - time.Second), // PID may have been reused
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := weaveRecoverAbandonedFinalizations(dir); err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 1); got == nil || got.State != "failed" || got.FinishedAt.IsZero() || got.Completion != "failed: finalizer exited before terminal evidence" {
		t.Fatalf("dead finalizer did not leave durable salvage evidence: %#v", got)
	}
	if got := findWeaveItem(q, 2); got == nil || got.State != "working" || got.Completion != "" || got.FinalizerPID != 0 {
		t.Fatalf("live wrapper was not released after finalizer interruption: %#v", got)
	}
}

func TestWeaveStatusObservesExpiredFinalizationAndDoctorRecovers(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:           1,
		State:        "finalizing",
		Completion:   "conductor-finalizing-observed-idle",
		FinalizerPID: os.Getpid(), // live/reused PID must not defeat an expired lease
		FinalizingAt: time.Now().UTC().Add(-weaveFinalizationLease - time.Second),
		Workspace:    root + "/preserve",
	}}}); err != nil {
		t.Fatal(err)
	}
	out, code := runWeave(t, "status", "1", "--json")
	if code != 0 || !strings.Contains(out, `"state": "finalizing"`) {
		t.Fatalf("status did not preserve finalizing state: exit=%d output=%s", code, out)
	}
	if out, code = runWeave(t, "doctor", "--json"); code != 0 || !strings.Contains(out, `"to": "failed"`) {
		t.Fatalf("doctor did not recover expired finalizing claim: exit=%d output=%s", code, out)
	}
}

func TestWeaveKillRecoversExpiredFinalizationWithoutList(t *testing.T) {
	root := setupIsolationFixture(t)
	workspace := t.TempDir() + "/workspace"
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	wrapper := exec.Command("sleep", "60")
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = wrapper.Process.Kill()
		_, _ = wrapper.Process.Wait()
	}()
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:           1,
		State:        "finalizing",
		Completion:   "conductor-finalizing-observed-idle",
		FinalizerPID: os.Getpid(),
		FinalizingAt: time.Now().UTC().Add(-weaveFinalizationLease - time.Second),
		WrapperPid:   wrapper.Process.Pid,
		Workspace:    workspace,
	}}}); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "kill", "1", "--json"); code != 0 {
		t.Fatalf("direct kill did not recover finalizing claim: exit=%d output=%s", code, out)
	}
	_ = wrapper.Wait()
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 1); got == nil || got.State != "killed" || got.Completion != "" {
		t.Fatalf("kill did not own recovered finalizing run: %#v", got)
	}
}

func TestWeaveAbandonRecoversExpiredFinalizationWithoutList(t *testing.T) {
	root := setupIsolationFixture(t)
	workspace := t.TempDir() + "/workspace"
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	wrapper := exec.Command("sleep", "60")
	if err := wrapper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = wrapper.Process.Kill()
		_, _ = wrapper.Process.Wait()
	}()
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:           1,
		State:        "finalizing",
		Completion:   "conductor-finalizing-observed-idle",
		FinalizerPID: os.Getpid(),
		FinalizingAt: time.Now().UTC().Add(-weaveFinalizationLease - time.Second),
		WrapperPid:   wrapper.Process.Pid,
		Workspace:    workspace,
	}}}); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "abandon", "1", "--json"); code != 0 {
		t.Fatalf("direct abandon did not recover finalizing claim: exit=%d output=%s", code, out)
	}
	_ = wrapper.Wait()
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("abandon kept workspace after stopping wrapper: %v", err)
	}
}

func TestWeaveFinalizeCountsAgainstImmutableBase(t *testing.T) {
	root := setupIsolationFixture(t)
	base := strings.TrimSpace(gitT(t, root, "rev-parse", "HEAD"))
	workspace := t.TempDir() + "/workspace"
	gitT(t, root, "clone", "--local", "--no-hardlinks", root, workspace)
	gitT(t, workspace, "checkout", "-qb", "agent/weave-issue-1")
	mustWrite(t, workspace+"/done.txt", "done\n")
	gitT(t, workspace, "add", "done.txt")
	gitT(t, workspace, "commit", "-qm", "agent complete")
	// Advance the live base to the worker head. Measuring against mutable main
	// would now report zero; the run's immutable BaseSHA still proves one commit.
	gitT(t, root, "fetch", workspace, "agent/weave-issue-1:agent/weave-issue-1")
	gitT(t, root, "merge", "--ff-only", "agent/weave-issue-1")
	t.Chdir(root)
	dir, _ := weaveQueueDir(root)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: root, Items: []*weaveItem{{
		ID:        1,
		State:     "working",
		Workspace: workspace,
		Branch:    "agent/weave-issue-1",
		BaseSHA:   base,
	}}}); err != nil {
		t.Fatal(err)
	}
	if out, code := runWeave(t, "finalize", "1", "--observed-idle", "--json"); code != 0 {
		t.Fatalf("finalize failed after base advanced: exit=%d output=%s", code, out)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := findWeaveItem(q, 1); got == nil || got.State != "submitted" || got.CommitsAhead != 1 {
		t.Fatalf("immutable base evidence was lost after live base advance: %#v", got)
	}
}

func TestWeaveFinalizeRequiresObservedIdleAcknowledgment(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)
	if _, code := runWeave(t, "add", "do not infer terminal state", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}
	out, code := runWeave(t, "finalize", "1", "--json")
	if code == 0 || !strings.Contains(out, "--observed-idle") {
		t.Fatalf("finalize without explicit acknowledgment must refuse: exit=%d output=%s", code, out)
	}
}
