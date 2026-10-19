# Sprint 467 / story #2380 (17246595f4c3) — weave run instances retired at terminal

Base: d661ccfc53c7e5bf74e40632d05d184220be4fad (agent/weave-issue-433). Host: dev-host (darwin), filtered tests only.

Fix: `saveWeaveQueue` calls `weaveReconcileRunInstances`, which retires (via
`agentlaunch.RetireContext` + `bus.InstanceMail`, mail archived) the instance of
every run that is terminal AND unresumable (done/abandoned, or workspace gone),
skipping any run whose wrapper/child/finalizer PID or owning session is live.
Resumable terminal runs keep their UUID. `InstanceRetired` on the item avoids
re-reading records. No cap bypass; cap stays non-fatal as before.

## RED (before fix, test file only)

`go test ./pkg/weave -run 'TestNinthSequentialRunOwnsAnIdentity|TestStaleTerminalRunInstancesAreReconciled|TestLiveRunInstanceIsNotRetired' -count=1`

```
--- FAIL: TestNinthSequentialRunOwnsAnIdentity (0.28s)
    weave_instance_retire_test.go:48: run 9 of one agent launched with no identity (family cap exhausted by finished runs)
--- FAIL: TestStaleTerminalRunInstancesAreReconciled (0.12s)
    weave_instance_retire_test.go:93: run #1 (done, workspace ""): active=true, want false
    weave_instance_retire_test.go:93: run #2 (abandoned, workspace ""): active=true, want false
    weave_instance_retire_test.go:93: run #3 (failed, workspace ""): active=true, want false
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	1.145s
FAIL
```

## GREEN (after fix)

Same command with `-v`:

```
=== RUN   TestNinthSequentialRunOwnsAnIdentity
--- PASS: TestNinthSequentialRunOwnsAnIdentity (0.31s)
=== RUN   TestStaleTerminalRunInstancesAreReconciled
--- PASS: TestStaleTerminalRunInstancesAreReconciled (0.10s)
=== RUN   TestLiveRunInstanceIsNotRetired
--- PASS: TestLiveRunInstanceIsNotRetired (0.03s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	1.181s
```

Also: `go test ./pkg/weave -run 'Instance|Retire|Handoff|Resume|Reset' -count=1` → ok (76.0s);
`go test ./pkg/weave -run 'Schema|JSON|Queue|Save|Field' -count=1` → ok; `go vet ./pkg/weave` clean.
Not run here (manager): whole-package `go test ./pkg/weave`, `scripts/crossvet.sh`.

## Follow-up (supervisor ruling MB6628): resumable completed runs must not count toward cap

Review of 4a573629: submitted/failed runs with their workspace kept stayed
ACTIVE and COUNTED, so the ninth such sequential run still lost its identity.

Fix (on top of 4a573629): `fleet.Instance.Parked` — an active context preserved
for its holder's resume but not executing. `Instance.Counted()` (active and not
parked) is what `Open` caps on. `InstanceStore.Park` (idempotent) and
`InstanceStore.Unpark` (re-admits against the cap under the same store lock
`Open` counts with; refusal leaves it parked). `agentlaunch` unparks on
explicit/self resume after claiming (claim released on refusal), and idle-reuse
skips parked contexts so another launch cannot take a weave run's context.
`weaveReconcileRunInstances` now PARKS terminal-resumable runs and RETIRES
unresumable ones, in both cases only when no wrapper/child/finalizer PID is
live and no live session owns the instance; it also runs before allocation on
`weave start` (both fresh and resume sites, under the existing queue lock). A
resume refused by the cap is reported, keeping the recorded UUID (no silent
fallback to a new/no identity). Generated KB.md change from c12180ff reverted.

### RED (test file only, on 4a573629/c12180ff; `Parked` field assertions stubbed since the field did not exist yet)

`go test ./pkg/weave/ -run 'TestNinthSequentialSubmittedRunOwnsAnIdentity|TestResumeOfParkedContextIsReadmittedAgainstCap|TestLiveResumableRunIsNotParked' -count=1`

```
--- FAIL: TestNinthSequentialSubmittedRunOwnsAnIdentity (0.23s)
    weave_instance_retire_test.go:146: run 9 of one agent launched with no identity (family cap exhausted by submitted runs)
--- FAIL: TestResumeOfParkedContextIsReadmittedAgainstCap (0.02s)
    weave_instance_retire_test.go:190: a parked context still counted toward the cap: err=<nil> instance=""
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	0.881s
FAIL
```

### GREEN

`go test ./pkg/weave/ -run 'TestNinthSequential|TestResumeOfParkedContext|TestLiveResumableRunIsNotParked|TestStaleTerminalRunInstances|TestLiveRunInstanceIsNotRetired|Instance' -count=1 -v` (PASS lines)

```
--- PASS: TestConductorRoles_CarryTheHolderInstance (0.00s)
--- PASS: TestStaleSeatIsRecoverableByTheSameInstanceOnANewSession (8.09s)
--- PASS: TestNinthSequentialRunOwnsAnIdentity (0.28s)
--- PASS: TestStaleTerminalRunInstancesAreReconciled (0.10s)
--- PASS: TestLiveRunInstanceIsNotRetired (0.03s)
--- PASS: TestNinthSequentialSubmittedRunOwnsAnIdentity (0.34s)
--- PASS: TestResumeOfParkedContextIsReadmittedAgainstCap (0.05s)
--- PASS: TestLiveResumableRunIsNotParked (0.05s)
--- PASS: TestWeaveRunRecordsAFreshInstance (0.01s)
--- PASS: TestWeaveResumePreservesTheRunInstance (0.02s)
--- PASS: TestWeaveInstanceEnvIsSilentForALegacyRun (0.00s)
--- PASS: TestWeaveRetireRunInstanceArchivesAndReleases (0.02s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	9.812s
```

- `go test ./pkg/fleet/ -run TestParkedInstance -count=1 -v` → `--- PASS: TestParkedInstanceIsNotCountedAndUnparkReadmits`
- `go test ./pkg/fleet/ -run 'Instance|Park|Retire|Resume|Cap|Label|Handle|UUID' -count=1` → `ok` (0.9s)
- `go test ./pkg/agentlaunch/ -run 'Context|Retire|Identity|Instance' -count=1` → `ok` (0.6s)
- `go test ./pkg/weave -run 'Instance|Retire|Handoff|Resume|Reset|Parked|Submitted' -count=1` → `ok` (53.0s)
- `go vet ./pkg/weave ./pkg/fleet ./pkg/agentlaunch` → clean

Not run here (manager): whole-package `go test` of pkg/{weave,fleet,agentlaunch}, `scripts/crossvet.sh`.

## Follow-up: owner check atomic with park/retire (manager review of 1667b775)

Concern: reconcile checked `principal.InstanceOwner` and then separately called
`Park`/`Retire`; a concurrent `agentlaunch.OpenContext` (resume or idle reuse)
could claim between the two, and a resume deciding on a pre-park read never
unparked.

Fix (no redesign, existing locks only):
- `fleet.InstanceStore.ParkIf` / `RetireIf` evaluate a guard UNDER the store
  lock on the re-read record (`RetireIf` also on the first read, before mail
  collection). `Park`/`Retire` are the nil-guard forms.
- `agentlaunch.ParkIdleContext` / `RetireIdleContext` pass a guard refusing ANY
  live owner (`ErrContextOwned`); weave reconcile uses them instead of the
  unlocked pre-check.
- `unparkClaimed` always re-reads under the store lock (was: trusted the
  caller's pre-claim snapshot); `Unpark` refuses a retired record; idle reuse
  also goes through `unparkClaimed`.
- Lock order: the guard calls `room.SessionOwner`, a lock-free card read, so
  the fleet store lock never nests the room claim lock.

Why it is exclusive: a claimer writes its claim (room lock) and THEN calls
`Unpark` (store lock). Reconcile's guard + write run inside one store-lock
critical section. If the claim lands before the guard, the guard refuses. If
after, the claimer's `Unpark` acquires the store lock after reconcile released
it, so it sees the park (re-admits against the cap, or refuses and releases the
claim) or the retirement (refuses and releases the claim). No end state is
"live owner + parked" or "live owner + retired".

RED (pre-fix, base 1667b775, runtime failures):

```
$ go test ./pkg/agentlaunch -run TestResumeRacingParkEndsCounted -count=1
--- FAIL: TestResumeRacingParkEndsCounted (0.06s)
    park_race_test.go:49: a live-owned context is parked (Parked="2026-10-18T13:20:47.423391Z"): it runs without counting against the cap
FAIL	github.com/qiangli/yoke/pkg/agentlaunch	0.326s
$ go test ./pkg/weave -run TestReconcileDoesNotRetireAnOwnerAcquiredDuringMailArchive -count=1
--- FAIL: TestReconcileDoesNotRetireAnOwnerAcquiredDuringMailArchive (0.03s)
    weave_instance_retire_test.go:261: retired an instance a live session claimed during the mail archive (retired="2026-10-18T13:20:59.188811Z")
FAIL	github.com/qiangli/yoke/pkg/weave	0.652s
```

(The RED step added only the tests plus the behavior-neutral
`weaveInstanceMail` seam, so the test can claim during mail collection.)

GREEN:

```
$ go test ./pkg/agentlaunch -run 'TestResumeRacingParkEndsCounted|Context|Resume|Idle|Cap|Retire' -count=1
ok  	github.com/qiangli/yoke/pkg/agentlaunch	2.412s
$ go test ./pkg/fleet -run 'Instance|Park|Retire|Cap' -count=1
ok  	github.com/qiangli/yoke/pkg/fleet	1.297s
$ go test ./pkg/weave -run 'TestReconcileDoesNotRetireAnOwnerAcquiredDuringMailArchive|Ninth|Stale|LiveRun|Instance|Retire|Park' -count=1
ok  	github.com/qiangli/yoke/pkg/weave	72.569s
$ go test ./pkg/agentlaunch -run 'TestResumeRacingParkEndsCounted|TestIdleGuard' -count=1 -v
--- PASS: TestResumeRacingParkEndsCounted (0.06s)
--- PASS: TestIdleGuardRefusesLiveOwnerAndRetiredResume (0.08s)
$ go vet ./pkg/fleet ./pkg/agentlaunch ./pkg/weave   # clean
```

Not run here (manager): whole-package tests and `scripts/crossvet.sh`.

## Follow-up: direct claimers excluded from guarded park/retire (manager blocker)

Blocker: `snapshot := store.Resume(id); RetireIdleContext(store, id, …);
principal.ClaimInstance(snapshot, liveClaim)` succeeded, and
`pkg/bus/actor.go:162` calls `ClaimInstance` directly (no OpenContext
claim-then-unpark), so the store-lock guard on park/retire did not exclude
every claimant.

Fix: every claim is now taken UNDER the fleet store lock on the persisted
record — `fleet.InstanceStore.Admit` (re-read → refuse retired → refuse
parked unless the caller re-admits against the cap → run the room claim →
clear Parked). `principal.ClaimInstanceIn(store, inst, claim, unpark)` routes
through it; `ClaimInstance` delegates with the host store. OpenContext and
`bus.resolveInstanceActor` use `ClaimInstanceIn`; `unparkClaimed` is gone.
`ParkIf`/`RetireIf` already evaluate the owner guard under that same lock,
so claim and guarded mutation are mutually exclusive. Lock order is store
lock → room members lock on both sides (the guard's `room.SessionOwner` takes
no lock); nothing takes the room lock and then the store lock, so no inversion.

RED (runtime, tree identical to bad8f14a, HEAD 03b0611f):

```
$ go test ./pkg/agentlaunch -run TestDirectClaimOfStaleSnapshotRefusedAfterRetire -count=1
--- FAIL: TestDirectClaimOfStaleSnapshotRefusedAfterRetire (0.05s)
    park_race_test.go:111: a direct claim on a stale snapshot was admitted onto a retired context
    park_race_test.go:114: the refused direct claim left a live owner on a retired context
FAIL	github.com/qiangli/yoke/pkg/agentlaunch	0.541s
```

GREEN (dev-host, filtered):

```
$ go test ./pkg/agentlaunch -run 'DirectClaim|OwnerAcquired|ResumeRacingPark|IdleGuard' -count=1 -v
--- PASS: TestResumeRacingParkEndsCounted
--- PASS: TestIdleGuardRefusesLiveOwnerAndRetiredResume
--- PASS: TestDirectClaimOfStaleSnapshotRefusedAfterRetire
--- PASS: TestDirectClaimConcurrentWithGuardedRetire   # real claim started while RetireIf holds the lock → serialized, refused
--- PASS: TestDirectClaimConcurrentWithGuardedPark      # unpark=false refused (ErrInstanceParked); unpark=true ends owned+counted
--- PASS: TestOwnerAcquiredDuringMailCollectionBlocksRetire
ok  	github.com/qiangli/yoke/pkg/agentlaunch	1.537s
$ go test ./pkg/agentlaunch -count=1                                     # ok 3.382s
$ go test ./pkg/principal ./pkg/fleet -count=1                           # ok / ok
$ go test ./pkg/bus -run 'Authored|Instance' -count=1                    # ok 0.896s
$ go test ./pkg/weave -run 'Ninth|Stale|LiveRun|Instance|Retire|Park|Reconcile' -count=1   # ok 34.760s
$ go vet ./pkg/agentlaunch ./pkg/principal ./pkg/fleet ./pkg/bus ./pkg/weave              # clean
```

Not run here (manager): whole `pkg/bus` / `pkg/weave` packages and `scripts/crossvet.sh`.

## Follow-up: review notes MB6783 (re-admit rollback, idle-reuse errors)

Base: 335fcb64 (clean delivery). Source: `pkg/principal/instance.go`
(ClaimInstanceIn releases ONLY a claim this attempt acquired when Admit fails
after the room claim, e.g. the un-park write), `pkg/fleet/instance.go`
(`ErrInstanceRetired` sentinel on Admit's retired refusal),
`pkg/agentlaunch/identity.go` (idle reuse skips only `*room.ErrLive`,
`ErrInstanceParked`, `ErrInstanceRetired`; other errors are returned).
Failure injection: store dir / room members dir chmod 0500 (skipped on
Windows/root).

RED (runtime, before the fix):

```
$ go test ./pkg/agentlaunch -run 'TestReadmitWriteFailure|TestIdleReuseReportsUnexpected' -count=1
--- FAIL: TestReadmitWriteFailureReleasesNewClaim
    park_race_test.go:263: parked record left live-owned by "session-b" after a failed re-admit
--- FAIL: TestIdleReuseReportsUnexpectedClaimError
    park_race_test.go:305: an unexpected claim error was swallowed: 2 active instances, want the 1 idle candidate
FAIL	github.com/qiangli/yoke/pkg/agentlaunch
```

(TestReadmitWriteFailureKeepsPriorClaim passed before and after: it pins that
the rollback never drops a claim the session already held.)

GREEN:

```
$ go test ./pkg/agentlaunch -run 'TestReadmitWriteFailure|TestIdleReuseReportsUnexpected' -count=1 -v
--- PASS: TestReadmitWriteFailureReleasesNewClaim
--- PASS: TestReadmitWriteFailureKeepsPriorClaim
--- PASS: TestIdleReuseReportsUnexpectedClaimError
$ go vet ./pkg/agentlaunch ./pkg/principal ./pkg/fleet                     # clean
$ go test ./pkg/agentlaunch -count=1                                        # ok
$ go test -run 'Instance|Claim|Park|Retire|Admit' ./pkg/fleet ./pkg/principal -count=1   # ok / ok
$ go test -run 'Actor|Instance|Claim' ./pkg/bus -count=1                    # ok
```

Not run here (manager): whole `pkg/principal`, `pkg/bus`, `pkg/weave` and `scripts/crossvet.sh`.
