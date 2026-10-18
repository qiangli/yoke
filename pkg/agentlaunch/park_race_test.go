package agentlaunch

// Story 17246595f4c3: a resume that read its context BEFORE a reconcile parked
// it must still end up re-admitted (counted) once it owns the context. A
// resume decides on a stale read, so the unpark has to re-read under the store
// lock rather than trust the snapshot.

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

func TestResumeRacingParkEndsCounted(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")
	first, err := OpenContext(l, req("session-a", store))
	if err != nil {
		t.Fatal(err)
	}
	ReleaseContext(first, "session-a")

	// The resume reads the context: not parked yet.
	stale, err := store.Resume(first.Instance.UUID)
	if err != nil {
		t.Fatal(err)
	}
	// A reconcile sees no owner and parks it.
	if err := store.Park(stale.UUID); err != nil {
		t.Fatal(err)
	}
	// The resume claims and re-admits from its stale read.
	if _, err := principal.ClaimInstanceIn(store, stale, principal.InstanceClaim{Session: "session-b", OwnerPID: os.Getpid()}, true); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(stale.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, owned := principal.InstanceOwner(rec); !owned {
		t.Fatal("the resuming session lost its claim")
	}
	if !rec.Counted() {
		t.Errorf("a live-owned context is parked (Parked=%q): it runs without counting against the cap", rec.Parked)
	}
}

// ParkIdleContext/RetireIdleContext refuse ANY live owner — the run's own
// session included — and a resume that read before a retirement is refused
// with its claim released rather than left owning a retired context.
func TestIdleGuardRefusesLiveOwnerAndRetiredResume(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")
	ctx, err := OpenContext(l, req("session-a", store))
	if err != nil {
		t.Fatal(err)
	}
	id := ctx.Instance.UUID
	mail := func(fleet.Instance) ([]string, error) { return nil, nil }
	if err := ParkIdleContext(store, id); !errors.Is(err, ErrContextOwned) {
		t.Errorf("park of a live-owned context: err=%v, want ErrContextOwned", err)
	}
	if _, err := RetireIdleContext(store, id, mail, "session-a"); !errors.Is(err, ErrContextOwned) {
		t.Errorf("retire of a live-owned context: err=%v, want ErrContextOwned", err)
	}
	if rec, _ := store.Get(id); !rec.Counted() {
		t.Fatalf("a refused park/retire mutated the record: %+v", rec)
	}
	ReleaseContext(ctx, "session-a")

	stale, _ := store.Resume(id)
	if _, err := RetireIdleContext(store, id, mail, "session-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := principal.ClaimInstanceIn(store, stale, principal.InstanceClaim{Session: "session-b", OwnerPID: os.Getpid()}, true); err == nil {
		t.Error("a resume that read before the retirement was admitted onto a retired context")
	}
	if _, owned := principal.InstanceOwner(stale); owned {
		t.Error("the refused resume kept its claim on a retired context")
	}
}

// A DIRECT claimer (bus.resolveInstanceActor calls principal.ClaimInstance,
// not OpenContext) holding a snapshot read before a guarded retirement must
// be refused, and must not be left owning the retired context.
func TestDirectClaimOfStaleSnapshotRefusedAfterRetire(t *testing.T) {
	store := identityHost(t)
	ctx, err := OpenContext(rawLaunch("claude:opus5.5"), req("session-a", store))
	if err != nil {
		t.Fatal(err)
	}
	ReleaseContext(ctx, "session-a")
	snapshot, err := store.Resume(ctx.Instance.UUID)
	if err != nil {
		t.Fatal(err)
	}
	mail := func(fleet.Instance) ([]string, error) { return nil, nil }
	if _, err := RetireIdleContext(store, snapshot.UUID, mail, "session-a"); err != nil {
		t.Fatal(err)
	}
	if err := principal.ClaimInstance(snapshot, principal.InstanceClaim{Session: "live", OwnerPID: os.Getpid()}); err == nil {
		t.Error("a direct claim on a stale snapshot was admitted onto a retired context")
	}
	if _, owned := principal.InstanceOwner(snapshot); owned {
		t.Error("the refused direct claim left a live owner on a retired context")
	}
}

// openIdle opens one context and releases it, returning a snapshot read
// before whatever the caller does next.
func openIdle(t *testing.T, store *fleet.InstanceStore) fleet.Instance {
	t.Helper()
	ctx, err := OpenContext(rawLaunch("claude:opus5.5"), req("session-a", store))
	if err != nil {
		t.Fatal(err)
	}
	ReleaseContext(ctx, "session-a")
	snap, err := store.Resume(ctx.Instance.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// claimDuring starts a real direct claim (the bus actor's path) and reports
// that it has NOT completed within a grace period — i.e. it is held off by the
// store lock the caller is inside.
func claimDuring(t *testing.T, store *fleet.InstanceStore, snap fleet.Instance, unpark bool) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := principal.ClaimInstanceIn(store, snap, principal.InstanceClaim{Session: "racer", OwnerPID: os.Getpid()}, unpark)
		done <- err
	}()
	select {
	case err := <-done:
		t.Errorf("the claim completed while a guarded mutation held the store lock (err=%v)", err)
		done <- err
	case <-time.After(200 * time.Millisecond):
	}
	return done
}

// A direct claim attempted WHILE a guarded retirement holds the store lock
// is serialized behind it and then refused: never a live owner on a retired
// context.
func TestDirectClaimConcurrentWithGuardedRetire(t *testing.T) {
	store := identityHost(t)
	snap := openIdle(t, store)
	mail := func(fleet.Instance) ([]string, error) { return nil, nil }
	var claimed <-chan error
	calls := 0
	guard := func(inst fleet.Instance) error {
		if calls++; calls == 2 { // the re-check under the store lock
			claimed = claimDuring(t, store, snap, true)
		}
		return idleGuard(inst)
	}
	if _, err := store.RetireIf(snap.UUID, mail, guard); err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		t.Fatal("guard never ran under the lock")
	}
	if err := <-claimed; err == nil {
		t.Error("a claim racing a guarded retirement was admitted onto the retired context")
	}
	if _, owned := principal.InstanceOwner(snap); owned {
		t.Error("a retired context has a live owner")
	}
}

// The same race against a park: a claim that does not ask to re-admit is
// refused; a resume that does ends owned AND counted, never owned-but-parked.
func TestDirectClaimConcurrentWithGuardedPark(t *testing.T) {
	for _, unpark := range []bool{false, true} {
		store := identityHost(t)
		snap := openIdle(t, store)
		var claimed <-chan error
		guard := func(inst fleet.Instance) error {
			claimed = claimDuring(t, store, snap, unpark)
			return idleGuard(inst)
		}
		if err := store.ParkIf(snap.UUID, guard); err != nil {
			t.Fatal(err)
		}
		err := <-claimed
		rec, gerr := store.Get(snap.UUID)
		if gerr != nil {
			t.Fatal(gerr)
		}
		_, owned := principal.InstanceOwner(rec)
		switch {
		case !unpark && !errors.Is(err, fleet.ErrInstanceParked):
			t.Errorf("unpark=false: err=%v, want ErrInstanceParked", err)
		case !unpark && owned:
			t.Error("unpark=false: a refused claim owns the parked context")
		case unpark && err != nil:
			t.Errorf("unpark=true: resume after park refused: %v", err)
		case unpark && (!owned || !rec.Counted()):
			t.Errorf("unpark=true: owned=%v counted=%v; want an owned, counted context", owned, rec.Counted())
		}
	}
}

// An owner acquired while the retirement is collecting mail (before the store
// lock) is seen by the guard under the lock: the retirement is refused.
func TestOwnerAcquiredDuringMailCollectionBlocksRetire(t *testing.T) {
	store := identityHost(t)
	snap := openIdle(t, store)
	mail := func(fleet.Instance) ([]string, error) {
		_, err := principal.ClaimInstanceIn(store, snap, principal.InstanceClaim{Session: "racer", OwnerPID: os.Getpid()}, true)
		return nil, err
	}
	if _, err := store.RetireIf(snap.UUID, mail, idleGuard); !errors.Is(err, ErrContextOwned) {
		t.Errorf("retire with an owner acquired mid-collection: err=%v, want ErrContextOwned", err)
	}
	if rec, _ := store.Get(snap.UUID); !rec.Active() {
		t.Error("a live-owned context was retired")
	}
}

// readOnlyDir makes dir unwritable for the rest of the test (restored at
// cleanup). Skipped where permission bits do not bind (Windows, root).
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission bits do not refuse writes here")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
}

// A re-admit whose room claim lands but whose un-park write fails must not
// leave the record parked AND live-owned (running uncounted): the claim this
// attempt took is released.
func TestReadmitWriteFailureReleasesNewClaim(t *testing.T) {
	store := identityHost(t)
	inst := openIdle(t, store)
	if err := store.Park(inst.UUID); err != nil {
		t.Fatal(err)
	}
	readOnlyDir(t, os.Getenv(fleet.InstanceDirEnv))
	if _, err := principal.ClaimInstanceIn(store, inst, principal.InstanceClaim{Session: "session-b", OwnerPID: os.Getpid()}, true); err == nil {
		t.Fatal("re-admit succeeded although the un-park could not be persisted")
	}
	rec, err := store.Get(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Parked == "" {
		t.Fatalf("record un-parked despite the failed write: %+v", rec)
	}
	if owner, owned := principal.InstanceOwner(rec); owned {
		t.Errorf("parked record left live-owned by %q after a failed re-admit", owner.SessionClaim)
	}
}

// A same-session re-claim that fails to persist keeps the claim the session
// already held: the rollback releases only what this attempt acquired.
func TestReadmitWriteFailureKeepsPriorClaim(t *testing.T) {
	store := identityHost(t)
	inst := openIdle(t, store)
	claim := principal.InstanceClaim{Session: "session-b", OwnerPID: os.Getpid()}
	if _, err := principal.ClaimInstanceIn(store, inst, claim, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Park(inst.UUID); err != nil { // unguarded: simulates a parked record we already own
		t.Fatal(err)
	}
	readOnlyDir(t, os.Getenv(fleet.InstanceDirEnv))
	if _, err := principal.ClaimInstanceIn(store, inst, claim, true); err == nil {
		t.Fatal("re-admit succeeded although the un-park could not be persisted")
	}
	if owner, owned := principal.InstanceOwner(inst); !owned || owner.SessionClaim != "session-b" {
		t.Errorf("a failed re-claim dropped the claim the session already held (owned=%v)", owned)
	}
}

// Idle reuse skips only a candidate that is busy, parked or retired; any
// other claim failure is reported instead of silently opening a fresh
// instance past it.
func TestIdleReuseReportsUnexpectedClaimError(t *testing.T) {
	store := identityHost(t)
	l := rawLaunch("claude:opus5.5")
	openIdle(t, store)
	readOnlyDir(t, filepath.Join(os.Getenv("BASHY_ROOM_DIR"), "members"))
	if _, err := OpenContext(l, req("session-b", store)); err == nil {
		t.Fatal("OpenContext succeeded although the room claim could not be written")
	}
	os.Chmod(filepath.Join(os.Getenv("BASHY_ROOM_DIR"), "members"), 0o700)
	all, err := store.ActiveInstances("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("an unexpected claim error was swallowed: %d active instances, want the 1 idle candidate", len(all))
	}
}
