//go:build !windows

package weave

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// recoveryFixture is one queue run holding a real reservation, in a real repo
// so the managed `weave kill` path can measure its workspace.
type recoveryFixture struct {
	dir, budget string
	hooks       WeaveResourceHooks
	births      map[int]string
}

func newRecoveryFixture(t *testing.T, it *weaveItem) *recoveryFixture {
	t.Helper()
	isolateResourceLifecycle(t)
	weaveChildStopGrace = time.Second
	t.Cleanup(func() { weaveChildStopGrace = 5 * time.Second })
	repo := t.TempDir()
	initMemoryTestRepo(t, repo)
	t.Chdir(repo)
	repo, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := weaveQueueDir(repo)
	if err != nil {
		t.Fatal(err)
	}
	f := &recoveryFixture{dir: dir, budget: filepath.Join(t.TempDir(), "budget.json"), births: map[int]string{}}
	gate := resourceTestGate(t, f.budget)
	// Birth identities come only from this table, and only while the PID is
	// alive: an exited process has no identity, exactly like the native probe.
	f.hooks = WeaveResourceHooks{Budget: gate, LookupIdentity: func(_ context.Context, pid int) (string, error) {
		if b, ok := f.births[pid]; ok && pidAlive(pid) {
			return b, nil
		}
		return "", errors.New("no such process")
	}}
	a, err := beginWeaveAdmission(context.Background(), WeaveResourceHooks{Budget: gate}, WeaveResourceDemand{Run: filepath.Join(dir, strconv.FormatInt(it.ID, 10))})
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper that owned this admission is gone; its renew loop stops but
	// nothing settles the reservation (finish never infers from owner death).
	a.cancel()
	<-a.done
	a.owner.Close()
	it.Workspace = repo
	it.ResourceReservationID, it.ResourceReservationOwner = a.request.ID, a.request.Owner
	if err := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *recoveryFixture) cmd() *cobra.Command {
	c := &cobra.Command{}
	WithWeaveResources(c, f.hooks)
	return c
}

func (f *recoveryFixture) item(t *testing.T, id int64) *weaveItem {
	t.Helper()
	q, err := loadWeaveQueue(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	return findWeaveItem(q, id)
}

// startGroup launches an isolated group whose leader is reaped concurrently,
// as init reaps an orphan whose wrapper died. A marker carries the PID of a
// background descendant so the test can prove the whole group stopped.
func startGroup(t *testing.T, script string) (*exec.Cmd, int, <-chan struct{}) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "descendant")
	c := exec.Command("/bin/sh", "-c", "sleep 30 & echo $! > \""+marker+"\"; "+script)
	weavePrepareOwnedChild(c)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = c.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); <-reaped })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return c, pid, reaped
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func groupGone(pgid int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(-pgid, 0) != syscall.ESRCH {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// Runs 37/40: the wrapper died, its recorded isolated child group survived.
// Kill must stop that group (leader and descendant) by verified birth identity
// and settle — and must not signal a peer that now holds the wrapper's PID.
func TestWeaveKillStopsVerifiedChildWhenWrapperDead(t *testing.T) {
	peer := exec.Command("/bin/sh", "-c", "sleep 30")
	weavePrepareOwnedChild(peer)
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-peer.Process.Pid, syscall.SIGKILL); _ = peer.Wait() })
	it := &weaveItem{ID: 37, State: "working", WrapperPid: peer.Process.Pid, WrapperStartID: "dead-wrapper-birth", ToolLaunchRecorded: true}
	f := newRecoveryFixture(t, it)
	f.births[peer.Process.Pid] = "peer-birth" // the wrapper's PID was reused
	child, descendant, _ := startGroup(t, "trap '' TERM; wait")
	f.births[child.Process.Pid] = "child-birth"
	it.ChildPID, it.ChildGroup, it.ChildStartID = child.Process.Pid, child.Process.Pid, "child-birth"
	_ = withWeaveQueueLock(f.dir, func(q *weaveQueue) error { *findWeaveItem(q, 37) = *it; return nil })

	if err := runWeaveKill(f.cmd(), 37, "wrapper dead", true, &weaveOutputFlags{}); err != nil {
		t.Fatal(err)
	}
	if !groupGone(child.Process.Pid) || pidAlive(descendant) {
		t.Fatal("verified child group or its descendant survived kill")
	}
	if !pidAlive(peer.Process.Pid) {
		t.Fatal("kill signalled the peer holding the reused wrapper PID")
	}
	got := f.item(t, 37)
	if got.State != "killed" || !got.ResourceTerminated || resourceReservationCount(t, f.budget) != 0 {
		t.Fatalf("kill did not settle the stopped group: %+v", got)
	}
	if _, err := os.Stat(got.Workspace); err != nil {
		t.Fatal("kill lost the workspace", err)
	}
}

// A run already recorded killed with a retained reservation (the observed
// interruption) is recoverable through the same managed kill.
func TestWeaveKillRetainedReservationStopsSurvivingGroup(t *testing.T) {
	it := &weaveItem{ID: 40, State: "killed", WrapperPid: 0, ToolLaunchRecorded: true}
	f := newRecoveryFixture(t, it)
	child, descendant, _ := startGroup(t, "wait")
	f.births[child.Process.Pid] = "child-birth"
	it.ChildPID, it.ChildGroup, it.ChildStartID = child.Process.Pid, child.Process.Pid, "child-birth"
	_ = withWeaveQueueLock(f.dir, func(q *weaveQueue) error { *findWeaveItem(q, 40) = *it; return nil })
	if err := runWeaveKill(f.cmd(), 40, "recover", true, &weaveOutputFlags{}); err != nil {
		t.Fatal(err)
	}
	if !groupGone(child.Process.Pid) || pidAlive(descendant) {
		t.Fatal("surviving group not stopped")
	}
	if got := f.item(t, 40); got.State != "killed" || !got.ResourceTerminated || resourceReservationCount(t, f.budget) != 0 {
		t.Fatalf("retained reservation not settled: %+v", got)
	}
}

// A live process at the recorded child PID with a different birth identity is
// someone else's: never signalled, reservation retained.
func TestWeaveKillRefusesReusedChildPID(t *testing.T) {
	it := &weaveItem{ID: 41, State: "killed", ToolLaunchRecorded: true}
	f := newRecoveryFixture(t, it)
	other, descendant, _ := startGroup(t, "wait")
	f.births[other.Process.Pid] = "someone-else"
	it.ChildPID, it.ChildGroup, it.ChildStartID = other.Process.Pid, other.Process.Pid, "child-birth"
	_ = withWeaveQueueLock(f.dir, func(q *weaveQueue) error { *findWeaveItem(q, 41) = *it; return nil })
	err := runWeaveKill(f.cmd(), 41, "recover", true, &weaveOutputFlags{})
	if err == nil {
		t.Fatal("kill accepted a reused child PID")
	}
	if !pidAlive(other.Process.Pid) || !pidAlive(descendant) {
		t.Fatal("kill signalled a reused PID's group")
	}
	if got := f.item(t, 41); got.ResourceTerminated || resourceReservationCount(t, f.budget) != 1 {
		t.Fatalf("reused PID released the reservation: %+v", got)
	}
}

// A leader that exited while descendants remain cannot be re-verified, so its
// group is not signalled; the reservation stays until the group is absent.
func TestWeaveKillRefusesUnverifiableLeaderWithDescendants(t *testing.T) {
	it := &weaveItem{ID: 42, State: "killed", ToolLaunchRecorded: true}
	f := newRecoveryFixture(t, it)
	child, descendant, reaped := startGroup(t, "exit 0")
	<-reaped
	it.ChildPID, it.ChildGroup, it.ChildStartID = child.Process.Pid, child.Process.Pid, "child-birth"
	_ = withWeaveQueueLock(f.dir, func(q *weaveQueue) error { *findWeaveItem(q, 42) = *it; return nil })
	if err := runWeaveKill(f.cmd(), 42, "recover", true, &weaveOutputFlags{}); err == nil {
		t.Fatal("kill signalled a group whose leader could not be verified")
	}
	if !pidAlive(descendant) || resourceReservationCount(t, f.budget) != 1 {
		t.Fatal("unverifiable group was signalled or settled")
	}
	_ = syscall.Kill(descendant, syscall.SIGKILL)
	if !groupGone(child.Process.Pid) {
		t.Skip("kernel retained the emptied group")
	}
	if err := runWeaveKill(f.cmd(), 42, "recover", true, &weaveOutputFlags{}); err != nil {
		t.Fatal(err)
	}
	if !f.item(t, 42).ResourceTerminated || resourceReservationCount(t, f.budget) != 0 {
		t.Fatal("absent group did not settle")
	}
}

// Run 56: the wrapper died during hydration, before the tool launch was
// recorded. The recorded provisioning group is the containment proof: while
// any member lives the reservation is retained; once absent, a managed command
// releases it without any child evidence.
func TestWeavePreToolProvisioningFailureRecovers(t *testing.T) {
	it := &weaveItem{ID: 56, State: "failed", LaunchPhase: "failed: hydrate submodules: exit status 141"}
	f := newRecoveryFixture(t, it)
	wrapper, hydration, _ := startGroup(t, "wait")
	it.ProvisionGroup = wrapper.Process.Pid
	_ = withWeaveQueueLock(f.dir, func(q *weaveQueue) error { *findWeaveItem(q, 56) = *it; return nil })
	ctx := f.cmd().Context()
	if err := weaveReconcileOwnedTree(ctx, f.dir, 56); err == nil || !strings.Contains(err.Error(), "provisioning group may still contain") {
		t.Fatalf("live provisioning subprocess accepted: %v", err)
	}
	if pidAlive(hydration) == false || resourceReservationCount(t, f.budget) != 1 {
		t.Fatal("reconcile signalled or released a live provisioning group")
	}
	_ = syscall.Kill(-wrapper.Process.Pid, syscall.SIGKILL)
	if !groupGone(wrapper.Process.Pid) {
		t.Skip("kernel retained the stopped test group")
	}
	if err := runWeaveKill(f.cmd(), 56, "recover failed launch", true, &weaveOutputFlags{}); err != nil {
		t.Fatal(err)
	}
	got := f.item(t, 56)
	if got.State != "failed" || !got.ResourceTerminated || got.ChildPID != 0 || got.ChildStartID != "" || resourceReservationCount(t, f.budget) != 0 {
		t.Fatalf("pre-tool launch did not settle cleanly: %+v", got)
	}
}

// A launch mark without child evidence, or no recorded provisioning group, is
// not pre-tool proof: the tool may have run.
func TestWeavePreToolRecoveryRequiresContainmentRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		it   *weaveItem
	}{
		{"tool launch recorded", &weaveItem{ID: 57, State: "failed", ProvisionGroup: deadPID(t), ToolLaunchRecorded: true}},
		{"no provisioning group", &weaveItem{ID: 57, State: "failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t, tc.it)
			if err := weaveReconcileOwnedTree(f.cmd().Context(), f.dir, 57); err == nil || !strings.Contains(err.Error(), "not recorded") {
				t.Fatalf("missing containment record accepted: %v", err)
			}
			if resourceReservationCount(t, f.budget) != 1 {
				t.Fatal("reservation released without proof")
			}
		})
	}
}

// A settlement that fails at the meter keeps the reservation and the recorded
// verification time; the retry presents the identical proof and settles once.
func TestWeaveSettlementRetryAfterFailure(t *testing.T) {
	it := &weaveItem{ID: 58, State: "killed", ToolLaunchRecorded: true}
	f := newRecoveryFixture(t, it)
	pid := deadPID(t)
	if !groupGone(pid) {
		t.Skip("kernel retained the test group")
	}
	it.ChildPID, it.ChildGroup, it.ChildStartID = pid, pid, "child-birth"
	_ = withWeaveQueueLock(f.dir, func(q *weaveQueue) error { *findWeaveItem(q, 58) = *it; return nil })
	cancelled, cancel := context.WithCancel(f.cmd().Context())
	cancel()
	if err := weaveReconcileOwnedTree(cancelled, f.dir, 58); err == nil {
		t.Fatal("settlement claimed success with an unreachable meter")
	}
	first := f.item(t, 58)
	if first.ResourceTerminated || first.ResourceVerifiedAt.IsZero() || resourceReservationCount(t, f.budget) != 1 {
		t.Fatalf("failed settlement lost the reservation or its proof time: %+v", first)
	}
	for i := 0; i < 2; i++ { // retry, then an idempotent repeat
		if err := weaveReconcileOwnedTree(f.cmd().Context(), f.dir, 58); err != nil {
			t.Fatal(err)
		}
	}
	got := f.item(t, 58)
	if !got.ResourceTerminated || !got.ResourceVerifiedAt.Equal(first.ResourceVerifiedAt) || resourceReservationCount(t, f.budget) != 0 {
		t.Fatalf("retry did not settle with the recorded proof: %+v", got)
	}
}

// The tool-launch mark is written by the owning wrapper before Start, and is
// cleared by every fresh admission.
func TestWeaveToolLaunchMarkOwnedByAdmission(t *testing.T) {
	isolateResourceLifecycle(t)
	dir := t.TempDir()
	a := &weaveAdmission{}
	a.request.ID = "r1"
	it := &weaveItem{ID: 1, State: "working", ToolLaunchRecorded: true}
	weaveRecordAdmission(it, a)
	if it.ToolLaunchRecorded {
		t.Fatal("fresh admission inherited a prior launch mark")
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{it}}); err != nil {
		t.Fatal(err)
	}
	if err := weaveRecordToolLaunch(dir, 1, &weaveAdmission{}); err == nil {
		t.Fatal("foreign admission marked the launch")
	}
	if err := weaveRecordToolLaunch(dir, 1, a); err != nil {
		t.Fatal(err)
	}
	q, _ := loadWeaveQueue(dir)
	if !q.Items[0].ToolLaunchRecorded {
		t.Fatal("launch mark not persisted")
	}
}
