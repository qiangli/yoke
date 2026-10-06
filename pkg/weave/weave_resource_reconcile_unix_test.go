//go:build !windows

package weave

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestWeaveKillFinalizesVerifiedOwnedGroup(t *testing.T) {
	isolateResourceLifecycle(t)
	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	t.Chdir(repo)
	dir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "budget.json")
	gate := resourceTestGate(t, path)
	a, err := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: gate}, WeaveResourceDemand{Run: filepath.Join(dir, "7")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(false, true)
	child := exec.Command("/bin/sh", "-c", "exit 0")
	weavePrepareOwnedChild(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if syscall.Kill(-pid, 0) != syscall.ESRCH {
		t.Skip("kernel retained the stopped test group")
	}
	it := &weaveItem{ID: 7, State: "working", Workspace: repo, ResourceReservationID: a.request.ID, ResourceReservationOwner: a.request.Owner, ChildPID: pid, ChildStartID: "fixture-birth", ChildGroup: pid}
	if err := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	WithWeaveResources(cmd, WeaveResourceHooks{Budget: gate})
	if err := runWeaveKill(cmd, 7, "test", true, &weaveOutputFlags{}); err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if q.Items[0].State != "killed" || !q.Items[0].ResourceTerminated || resourceReservationCount(t, path) != 0 {
		t.Fatalf("kill did not settle verified owned group: %+v", q.Items[0])
	}
}

func TestWeaveMissingBirthLookupLeavesNoReconciliationProof(t *testing.T) {
	isolateResourceLifecycle(t)
	dir := t.TempDir()
	hooks := WeaveResourceHooks{LookupIdentity: func(context.Context, int) (string, error) { return "", errors.New("observer unavailable") }}
	cmd := exec.Command("/bin/sh", "-c", "sleep 1")
	weavePrepareOwnedChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	ctx := context.WithValue(context.Background(), weaveResourceKey{}, hooks)
	if err := weaveRecordOwnedChild(ctx, dir, 1, cmd, &weaveAdmission{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "queue.json")); !os.IsNotExist(err) {
		t.Fatalf("unverified child was recorded: %v", err)
	}
}

func TestWeaveReconcileRequiresRecordedOwnedGroup(t *testing.T) {
	isolateResourceLifecycle(t)
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "budget.json")
	gate := resourceTestGate(t, path)
	a, err := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: gate}, WeaveResourceDemand{Run: filepath.Join(dir, "39")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(false, true)
	it := &weaveItem{ID: 39, State: "killed", ResourceReservationID: a.request.ID, ResourceReservationOwner: a.request.Owner}
	if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), weaveResourceKey{}, WeaveResourceHooks{Budget: gate})
	err = weaveReconcileOwnedTree(ctx, dir, 39)
	if err == nil || !strings.Contains(err.Error(), "not recorded") {
		t.Fatalf("missing actionable refusal: %v", err)
	}
	if resourceReservationCount(t, path) != 1 {
		t.Fatal("legacy reservation was released")
	}
}

func TestWeaveReconcileWaitsForRecordedChildGroup(t *testing.T) {
	isolateResourceLifecycle(t)
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "budget.json")
	gate := resourceTestGate(t, path)
	hooks := WeaveResourceHooks{Budget: gate, LookupIdentity: func(context.Context, int) (string, error) { return "fixture-child-birth", nil }}
	a, err := beginWeaveAdmission(context.Background(), hooks, WeaveResourceDemand{Run: filepath.Join(dir, "7")})
	if err != nil {
		t.Fatal(err)
	}
	defer a.finish(false, true)
	it := &weaveItem{ID: 7, State: "working", ResourceReservationID: a.request.ID, ResourceReservationOwner: a.request.Owner}
	if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	weavePrepareOwnedChild(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	ctx := context.WithValue(context.Background(), weaveResourceKey{}, hooks)
	if err := weaveRecordOwnedChild(ctx, dir, 7, cmd, a); err != nil {
		t.Fatal(err)
	}
	if err := weaveReconcileOwnedTree(ctx, dir, 7); err == nil || !strings.Contains(err.Error(), "may still contain") {
		t.Fatalf("live group accepted: %v", err)
	}
	if resourceReservationCount(t, path) != 1 {
		t.Fatal("live child lost admission")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	deadline := time.Now().Add(time.Second)
	for syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := weaveReconcileOwnedTree(ctx, dir, 7); err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Items[0].ResourceTerminated || resourceReservationCount(t, path) != 0 {
		t.Fatal("verified tree stop was not settled")
	}
}
