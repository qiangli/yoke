package weave

// A run's instance is retired when the run reaches a terminal state it can no
// longer be resumed from — otherwise every finished run keeps an active
// instance and the family cap (default 8) refuses the ninth (story
// 17246595f4c3). These cover the sequential launches, the reconcile of
// already-stale instances, and the live-owner protection.

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

func retireRun(t *testing.T) (*weaveAgentLaunch, string, *fleet.InstanceStore) {
	t.Helper()
	_, l := instanceRun(t)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv(fleet.InstanceCapEnv, "")
	return l, t.TempDir(), fleet.NewInstanceStore(os.Getenv(fleet.InstanceDirEnv))
}

// openAndExit opens a run's instance and then releases its session the way a
// wrapper that exited leaves it: no live owner.
func openAndExit(t *testing.T, it *weaveItem, l *weaveAgentLaunch) {
	t.Helper()
	ctx, err := weaveOpenRunInstance(it, l, it.Branch)
	if err != nil {
		t.Fatal(err)
	}
	agentlaunch.ReleaseContext(ctx, it.SessionClaim)
}

// The ninth sequential run of one agent launches WITHOUT a cap override and
// owns a valid identity, because each finished run's instance was retired.
func TestNinthSequentialRunOwnsAnIdentity(t *testing.T) {
	l, dir, store := retireRun(t)
	q := &weaveQueue{Root: dir}
	for i := 1; i <= 9; i++ {
		it := &weaveItem{ID: int64(i), Title: "seq", Owner: "claude-a", State: "working",
			Branch: fmt.Sprintf("agent/weave-issue-%d", i), Workspace: t.TempDir()}
		q.Items = append(q.Items, it)
		openAndExit(t, it, l)
		if it.Instance == "" {
			t.Fatalf("run %d of one agent launched with no identity (family cap exhausted by finished runs)", i)
		}
		if _, err := store.Get(it.Instance); err != nil {
			t.Fatalf("run %d recorded an instance with no record: %v", i, err)
		}
		// The run lands: pull marks it done and drops the workspace.
		it.State, it.Workspace = "done", ""
		if err := saveWeaveQueue(dir, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, it := range q.Items {
		if rec, _ := store.Get(it.Instance); rec.Active() {
			t.Errorf("run #%d is done but its instance %s is still active", it.ID, it.Instance)
		}
	}
}

// Instances left active by runs that already ended (the stale backlog) are
// reconciled on the next queue write; a terminal run that can still be
// resumed (workspace on disk) keeps its context.
func TestStaleTerminalRunInstancesAreReconciled(t *testing.T) {
	l, dir, store := retireRun(t)
	done := &weaveItem{ID: 1, Owner: "claude-a", State: "done", Branch: "agent/weave-issue-1"}
	abandoned := &weaveItem{ID: 2, Owner: "claude-a", State: "abandoned", Branch: "agent/weave-issue-2"}
	failedGone := &weaveItem{ID: 3, Owner: "claude-a", State: "failed", Branch: "agent/weave-issue-3"}
	failedResumable := &weaveItem{ID: 4, Owner: "claude-a", State: "failed", Branch: "agent/weave-issue-4", Workspace: t.TempDir()}
	working := &weaveItem{ID: 5, Owner: "claude-a", State: "working", Branch: "agent/weave-issue-5"}
	q := &weaveQueue{Root: dir}
	for _, it := range []*weaveItem{done, abandoned, failedGone, failedResumable, working} {
		openAndExit(t, it, l)
		q.Items = append(q.Items, it)
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		it     *weaveItem
		active bool
	}{{done, false}, {abandoned, false}, {failedGone, false}, {failedResumable, true}, {working, true}} {
		rec, err := store.Get(c.it.Instance)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Active() != c.active {
			t.Errorf("run #%d (%s, workspace %q): active=%v, want %v", c.it.ID, c.it.State, c.it.Workspace, rec.Active(), c.active)
		}
	}
	// A resumable run keeps the same identity on resume.
	uuid := failedResumable.Instance
	openAndExit(t, failedResumable, l)
	if failedResumable.Instance != uuid {
		t.Errorf("resume changed the identity: %s -> %s", uuid, failedResumable.Instance)
	}
}

// A terminal run is NEVER retired while its wrapper/child is alive or another
// session still owns its instance.
func TestLiveRunInstanceIsNotRetired(t *testing.T) {
	l, dir, store := retireRun(t)
	livePid := &weaveItem{ID: 1, Owner: "claude-a", State: "done", Branch: "agent/weave-issue-1"}
	openAndExit(t, livePid, l)
	livePid.WrapperPid = os.Getpid()

	liveOwner := &weaveItem{ID: 2, Owner: "claude-a", State: "done", Branch: "agent/weave-issue-2"}
	if _, err := weaveOpenRunInstance(liveOwner, l, liveOwner.Branch); err != nil {
		t.Fatal(err) // session held by this (live) process
	}
	q := &weaveQueue{Root: dir, Items: []*weaveItem{livePid, liveOwner}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	for _, it := range q.Items {
		if rec, _ := store.Get(it.Instance); !rec.Active() {
			t.Errorf("run #%d was retired while still live", it.ID)
		}
	}
}

// Supervisor ruling MB6628: a run that ends SUBMITTED (or failed) with its
// workspace kept is resumable, so its context — UUID, mail, ownership — is
// preserved; but a completed worker is not executing and must not count
// toward the family cap. Nine such sequential runs of one agent each own a
// valid identity without a cap override, and every earlier context still
// resumes under its own UUID.
func TestNinthSequentialSubmittedRunOwnsAnIdentity(t *testing.T) {
	l, dir, store := retireRun(t)
	q := &weaveQueue{Root: dir}
	for i := 1; i <= 9; i++ {
		it := &weaveItem{ID: int64(i), Title: "seq", Owner: "claude-a", State: "working",
			Branch: fmt.Sprintf("agent/weave-issue-%d", i), Workspace: t.TempDir()}
		q.Items = append(q.Items, it)
		if err := saveWeaveQueue(dir, q); err != nil { // start reconciles before allocating
			t.Fatal(err)
		}
		openAndExit(t, it, l)
		if it.Instance == "" {
			t.Fatalf("run %d of one agent launched with no identity (family cap exhausted by submitted runs)", i)
		}
		if _, err := store.Get(it.Instance); err != nil {
			t.Fatalf("run %d recorded an instance with no record: %v", i, err)
		}
		it.State = "submitted" // workspace kept: resumable
		if err := saveWeaveQueue(dir, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, it := range q.Items {
		rec, err := store.Get(it.Instance)
		if err != nil || !rec.Active() {
			t.Errorf("submitted run #%d lost its resumable context %s (err %v)", it.ID, it.Instance, err)
		}
	}
	// Same-holder resume continues the same UUID and is re-admitted.
	first := q.Items[0]
	uuid := first.Instance
	if _, err := weaveOpenRunInstance(first, l, first.Branch); err != nil {
		t.Fatal(err)
	}
	if first.Instance != uuid {
		t.Errorf("resume changed the identity: %s -> %s", uuid, first.Instance)
	}
	if rec, _ := store.Get(uuid); rec.Parked != "" {
		t.Errorf("a resumed context is still parked: it would not count toward the cap while executing")
	}
}

// Resume re-admits against the cap: a parked context cannot become a second
// executing context past the cap; the refusal keeps the UUID for later.
func TestResumeOfParkedContextIsReadmittedAgainstCap(t *testing.T) {
	l, dir, store := retireRun(t)
	t.Setenv(fleet.InstanceCapEnv, "1")
	parked := &weaveItem{ID: 1, Owner: "claude-a", State: "working", Branch: "agent/weave-issue-1", Workspace: t.TempDir()}
	openAndExit(t, parked, l)
	parked.State = "failed"
	q := &weaveQueue{Root: dir, Items: []*weaveItem{parked}}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	busy := &weaveItem{ID: 2, Owner: "claude-a", State: "working", Branch: "agent/weave-issue-2", Workspace: t.TempDir()}
	if _, err := weaveOpenRunInstance(busy, l, busy.Branch); err != nil || busy.Instance == "" {
		t.Fatalf("a parked context still counted toward the cap: err=%v instance=%q", err, busy.Instance)
	}
	uuid := parked.Instance
	_, err := weaveOpenRunInstance(parked, l, parked.Branch)
	var capErr *fleet.CapError
	if !errors.As(err, &capErr) {
		t.Fatalf("resume past the cap was admitted (err %v)", err)
	}
	if parked.Instance != uuid {
		t.Errorf("a refused resume dropped the recorded UUID: %q", parked.Instance)
	}
	if rec, _ := store.Get(uuid); !rec.Active() || rec.Parked == "" {
		t.Errorf("a refused resume changed the parked context: %+v", rec)
	}
	// An unrelated non-fresh launch must not take over the parked context.
	ctx, err := agentlaunch.OpenContext(agentlaunch.Launch(*l), agentlaunch.ContextRequest{Session: "other-session", Store: store.WithCap(-1)})
	if err == nil && ctx.Instance.UUID == uuid {
		t.Errorf("idle reuse took over a parked weave context")
	}
}

// Reconcile is idempotent and never parks a run whose wrapper/child/finalizer
// or owning session is still live.
func TestLiveResumableRunIsNotParked(t *testing.T) {
	l, dir, store := retireRun(t)
	child := &weaveItem{ID: 1, Owner: "claude-a", State: "submitted", Branch: "agent/weave-issue-1", Workspace: t.TempDir()}
	openAndExit(t, child, l)
	child.ChildPID = os.Getpid()
	fin := &weaveItem{ID: 2, Owner: "claude-a", State: "failed", Branch: "agent/weave-issue-2", Workspace: t.TempDir()}
	openAndExit(t, fin, l)
	fin.FinalizerPID = os.Getpid()
	owned := &weaveItem{ID: 3, Owner: "claude-a", State: "submitted", Branch: "agent/weave-issue-3", Workspace: t.TempDir()}
	if _, err := weaveOpenRunInstance(owned, l, owned.Branch); err != nil {
		t.Fatal(err)
	}
	q := &weaveQueue{Root: dir, Items: []*weaveItem{child, fin, owned}}
	for i := 0; i < 2; i++ {
		if err := saveWeaveQueue(dir, q); err != nil {
			t.Fatal(err)
		}
	}
	for _, it := range q.Items {
		if rec, _ := store.Get(it.Instance); !rec.Active() || rec.Parked != "" {
			t.Errorf("live run #%d was parked or retired: %+v", it.ID, rec)
		}
	}
}

// A session that claims the instance WHILE a reconcile is collecting its mail
// (outside the store lock) is a live owner by the time the record would be
// written: the retirement must not land.
func TestReconcileDoesNotRetireAnOwnerAcquiredDuringMailArchive(t *testing.T) {
	l, dir, store := retireRun(t)
	it := &weaveItem{ID: 1, Owner: "claude-a", State: "working", Branch: "agent/weave-issue-1"}
	openAndExit(t, it, l)
	it.State = "done"
	prev := weaveInstanceMail
	t.Cleanup(func() { weaveInstanceMail = prev })
	weaveInstanceMail = func(inst fleet.Instance) ([]string, error) {
		if err := principal.ClaimInstance(inst, principal.InstanceClaim{Session: "foreign", OwnerPID: os.Getpid()}); err != nil {
			t.Fatal(err)
		}
		return prev(inst)
	}
	weaveReconcileRunInstances(&weaveQueue{Root: dir, Items: []*weaveItem{it}})
	rec, err := store.Get(it.Instance)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Active() || it.InstanceRetired {
		t.Errorf("retired an instance a live session claimed during the mail archive (retired=%q)", rec.Retired)
	}
}
