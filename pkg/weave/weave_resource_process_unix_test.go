//go:build !windows

package weave

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWeaveResourceOwnedChildTree(t *testing.T) {
	isolateResourceLifecycle(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30 & wait")
	weavePrepareOwnedChild(cmd)
	weaveConfigureOwnedCancellation(cmd)
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	if weaveOwnedChildTerminated(cmd) {
		t.Fatal("live tree declared terminated")
	}
	cancel()
	_ = cmd.Wait()
	// A kernel may retain a zombie briefly. The predicate must be conservative.
	if !weaveOwnedChildTerminated(cmd) && syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH {
		t.Fatal("gone group not detected")
	}
}
func TestWeaveResourceGrandchildRetainsClaim(t *testing.T) {
	isolateResourceLifecycle(t)
	marker := filepath.Join(t.TempDir(), "pid")
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 30 & echo $! > \""+marker+"\"")
	weavePrepareOwnedChild(cmd)
	weaveConfigureOwnedCancellation(cmd)
	if e := cmd.Run(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	if weaveOwnedChildTerminated(cmd) {
		t.Fatal("direct Wait falsely proved surviving grandchild gone")
	}
	path := filepath.Join(t.TempDir(), "budget.json")
	a, e := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: resourceTestGate(t, path)}, WeaveResourceDemand{Run: "grandchild"})
	if e != nil {
		t.Fatal(e)
	}
	if e = a.finish(weaveOwnedChildTerminated(cmd), true); e != nil {
		t.Fatal(e)
	}
	if resourceReservationCount(t, path) != 1 {
		t.Fatal("grandchild freed capacity")
	}
}
func TestWeaveResourceWrapperActualStartHelper(t *testing.T) {
	if os.Getenv("WEAVE_ACTUAL_START_HELPER") == "" {
		return
	}
	t.Setenv("HOME", os.Getenv("WEAVE_ACTUAL_START_HOME"))
	t.Setenv("USERPROFILE", os.Getenv("WEAVE_ACTUAL_START_HOME"))
	repo := os.Getenv("WEAVE_ACTUAL_START_REPO")
	t.Chdir(repo)
	cmd := NewWeaveCmd()
	WithWeaveResources(cmd, WeaveResourceHooks{LookupIdentity: func(context.Context, int) (string, error) { return "fixture-wrapper-birth", nil }})
	ready := filepath.Join(repo, "child-ready")
	finish := filepath.Join(repo, "child-finish")
	cmd.SetArgs([]string{"start", "--run", "1", "--pty", "never", "--max-runtime", "10s", "--", "/bin/sh", "-c", "echo progress > progress.txt; echo ready > \"" + ready + "\"; while [ ! -e \"" + finish + "\" ]; do sleep 0.1; done"})
	if e := cmd.Execute(); e != nil && os.Getenv("WEAVE_ACTUAL_START_PAUSE") == "" {
		t.Fatal(e)
	}
}
func TestWeaveResourceWrapperOwnsActualLaunch(t *testing.T)     { testWeaveResourceActual(t, false) }
func TestWeaveResourceOwnedPausePreservesProgress(t *testing.T) { testWeaveResourceActual(t, true) }
func testWeaveResourceActual(t *testing.T, pause bool) {
	isolateResourceLifecycle(t)
	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	repo, e := weaveRepoRoot(repo)
	if e != nil {
		t.Fatal(e)
	}
	dir, e := weaveQueueDir(repo)
	if e != nil {
		t.Fatal(e)
	}
	if e = saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{{ID: 1, State: "todo", Title: "fixture"}}}); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "budget.json")
	t.Setenv("BASHY_LLM_BUDGET_STATE", path)
	worker := exec.Command(os.Args[0], "-test.run=^TestWeaveResourceWrapperActualStartHelper$")
	if pause {
		t.Setenv("WEAVE_ACTUAL_START_PAUSE", "1")
	}
	worker.Env = append(os.Environ(), "WEAVE_ACTUAL_START_HELPER=1", "WEAVE_ACTUAL_START_REPO="+repo, "WEAVE_ACTUAL_START_HOME="+os.Getenv("HOME"))
	out := filepath.Join(t.TempDir(), "worker.log")
	log, e := os.Create(out)
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	worker.Stdout = log
	worker.Stderr = log
	if e = worker.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = worker.Process.Kill(); _ = worker.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, e = os.Stat(filepath.Join(repo, "child-ready")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(out)
			t.Fatalf("actual child never started: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	q, e := loadWeaveQueue(dir)
	if e != nil {
		t.Fatal(e)
	}
	it := q.Items[0]
	if it.WrapperPid != worker.Process.Pid || it.WrapperStartID != "fixture-wrapper-birth" || it.ResourceReservationID == "" {
		t.Fatalf("not owned by persistent wrapper: %+v", it)
	}
	if resourceReservationCount(t, path) != 1 {
		t.Fatal("actual child lacks reservation")
	}
	if pause {
		t.Chdir(repo)
		t.Setenv("WEAVE_CONDUCTOR", it.Owner)
		pauseCmd := NewWeaveCmd()
		WithWeaveResources(pauseCmd, WeaveResourceHooks{LookupIdentity: func(context.Context, int) (string, error) { return "fixture-wrapper-birth", nil }})
		pauseCmd.SetArgs([]string{"pause", "--reason", "fixture pressure relief"})
		if e = pauseCmd.Execute(); e != nil {
			t.Fatal(e)
		}
	} else if e = os.WriteFile(filepath.Join(repo, "child-finish"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Wait() }()
	select {
	case e = <-done:
		if e != nil {
			b, _ := os.ReadFile(out)
			t.Fatalf("wrapper failed: %v %s", e, b)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("wrapper failed to finish")
	}
	if pause {
		q, e := loadWeaveQueue(dir)
		if e != nil {
			t.Fatal(e)
		}
		if q.Items[0].State != "paused" {
			t.Fatalf("pause not checkpointed: %+v", q.Items[0])
		}
		if _, e = os.Stat(filepath.Join(it.Workspace, "progress.txt")); e != nil {
			t.Fatal("pause lost progress", e)
		}
		return
	}
	if resourceReservationCount(t, path) != 0 {
		t.Fatal("verified synchronous run did not settle")
	}
}
