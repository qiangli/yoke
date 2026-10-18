# EVIDENCE — Story d8b12042a77b (#2368, Sprint #467)

sprint instruct times out on a hung managed manager session.

## Scope ACKs (manager review 00:43 / 00:53 PDT)
- NO new `--recover` flag (WIP flag removed; `TestSprintInstructHasNoRecoverFlag` pins it).
- Recovery uses the EXISTING owner lifecycle: `StopSprintOwner` (retireSprintOwnerSession) then
  `StartSprintOwner` (ensureSprintOwnerSession), under the per-sprint owner-lifecycle lock instruct already holds.
- Detection is reported to the sprint thread as a `stage` entry (→ announced to seated conductors on the
  board + host event log) AND sent by bus to the instructing supervisor identity; the report carries the
  lease heartbeat age (heartbeat/progress requirement).
- Isolated fixture only: a temp-dir unix socket that accepts and never answers; HOME/BASHY_SPRINT_DIR/
  BASHY_ROOM_DIR are temp; bus send + board announce are stubbed. No real session touched.

## Change
- `pkg/weave/sprint_owner_stall.go`: `sprintOwnerStalled` (timeout after successful dial) +
  `recoverStalledSprintOwner` (report → stop → relaunch once; NEVER relaunch if stop fails; refuse
  self-restart from inside the owner's session) + `reportSprintOwnerStall`.
- `pkg/weave/weave_story_box.go`: instruct calls it on a stall (3 lines). No weave_impl.go edits.

## RED (tests on base 4076e8d4, pre-WIP)
`go test ./pkg/weave -run 'TestSprintInstruct(StalledManager|OrdinaryFailure|HasNoRecoverFlag)' -count=1`
```
--- FAIL: TestSprintInstructStalledManagerIsReportedAndRecovered (14.13s)
    sprint_owner_stall_test.go:206: stalled manager was not recovered: exit=1: sprint instruct: sprint #1 instruction was not delivered: deliver instruction to sprint-1-manager: read unix ->/var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/stall3172116532/ctl.sock: i/o timeout
--- FAIL: TestSprintInstructStalledManagerThatCannotStopIsNotRelaunched (26.98s)
    sprint_owner_stall_test.go:239: instruct error does not name the stall: sprint instruct: sprint #1 instruction was not delivered: deliver instruction to sprint-1-manager: read unix ->/var/folders/vg/nlsn8n8x77n1xgg2nlpnvz180000gn/T/stall1980055265/ctl.sock: i/o timeout
    sprint_owner_stall_test.go:242: stops=0 launches=1; a second manager must not launch beside the hung one
FAIL
FAIL	github.com/qiangli/yoke/pkg/weave	96.718s
FAIL
```
(OrdinaryFailure / HasNoRecoverFlag are guard tests and pass on base by design.)

## GREEN (this candidate)
```
=== RUN   TestSprintInstructStalledManagerIsReportedAndRecovered
--- PASS: TestSprintInstructStalledManagerIsReportedAndRecovered (32.00s)
=== RUN   TestSprintInstructStalledManagerThatCannotStopIsNotRelaunched
--- PASS: TestSprintInstructStalledManagerThatCannotStopIsNotRelaunched (12.15s)
=== RUN   TestSprintInstructOrdinaryFailureIsNotTreatedAsStall
--- PASS: TestSprintInstructOrdinaryFailureIsNotTreatedAsStall (14.10s)
=== RUN   TestSprintInstructHasNoRecoverFlag
--- PASS: TestSprintInstructHasNoRecoverFlag (8.48s)
PASS
ok  	github.com/qiangli/yoke/pkg/weave	67.570s
```
`go vet ./pkg/weave` clean; gofmt clean. Whole-package gate: manager (remote).

## Known limit (bashy side, not in this repo)
bashy's `stopSprintOwner` stops via the same control socket; a truly hung foreman may therefore refuse to
stop. This change then reports "recovery failed … no replacement was launched" to thread + supervisor
(safe: no second manager). Forcing termination of a hung foreman pid is a bashy agentos follow-up.

## 01:08 review — remaining plan (sent to s467-manager before implementation)
Receipt: ac42f308 NOT accepted; preserved locally. ACK: no new flag, existing StopSprintOwner/StartSprintOwner
lifecycle, notify actual supervisor, isolated fixtures only, no real-session kills. Final delivery = one squashed
commit on 4076e8d4 without KB.md / WIP ancestors, with Agent trailer.

Root cause of the gap: bashy `stopSprintOwnerForeman` = `foreman.SendCommand(stop)` over the SAME hung ctl.sock →
i/o timeout → card live → `stopSprintOwner` returns the error, so recovery always refuses.

1. yoke `pkg/foreman`: serve persists its own pid in State (`ServePID`); `foreman.Terminate(root,id)` escalates ONLY
   when proven-owned (state not stopped, ServePID alive, room card mode=foreman task=id held by that pid);
   SIGTERM → grace → SIGKILL; mark Stopped (reason stalled); remove socket. Any proof failure → refuse, no kill.
   Red/green with a re-exec'd fixture child that listens on ctl.sock and never answers + negative (unproven pid →
   refused, child still alive).
2. bashy seam (needs isolated checkout): `stopSprintOwnerForeman` falls back to `foreman.Terminate` only on a net
   timeout from SendCommand. One hunk + test.
3. Heartbeat/progress without an instruct: existing `weave immune` sweep (quota-alarm dedupe pattern) — a doing
   sprint whose managed owner seat is stale/unknown → one stage entry on the thread + bus alarm.
   OPEN: recipient. The lease holder IS the hung manager. Proposal: the thread's start author if != owner, else
   role:conductor announce.

## Step 1 — yoke pkg/foreman Terminate (proven-owned hung serve)
Fixture: this test binary re-exec'd as a serve process of a temp store; it stamps ServePID/ServeStart
exactly as ServeControl does and holds a ctl.sock that accepts and never answers. No real session.
RED (reproducer half of TestTerminateStopsAProvenOwnedHungServe): `SendCommand(stop)` over the hung
socket → `i/o timeout`, the fixture stays alive — the existing stop lifecycle cannot reach it.
GREEN: `go test ./pkg/foreman -run 'TestTerminate' -count=1 -v`
```
--- PASS: TestTerminateStopsAProvenOwnedHungServe (3.06s)
--- PASS: TestTerminateRefusesAPidItCannotProve (0.20s)
ok  	github.com/qiangli/yoke/pkg/foreman	3.819s
```
Proof gate: not stopped, ServePID alive, != caller, kernel start time (sysctl kern.proc.pid /
/proc/<pid>/stat f22) matches what serve recorded; else ErrServeNotProven and nothing is signalled.

## Step 2 — bashy stopSprintOwnerForeman timeout seam (isolated /tmp/s467-stall-bashy-394 @0d2af687)
Local commit a1bcb1a3 (NOT pushed). Built against this yoke tree via an out-of-tree
`GOWORK=/tmp/s467-stall-394.work` (bashy + this workspace; ollama/podman replaces restated) — not committed.
yoke: `processStart` exported as `foreman.ProcessStart` so the bashy fixture child stamps itself like ServeControl.
Fixture: bashy test binary re-exec'd (`-test.run=^TestHungSprintOwnerServeHelper$`) as a hung serve of a
temp BASHY_FOREMAN_DIR session + a live room card (temp BASHY_ROOM_DIR) with the child's pid.
RED (seam unchanged): `GOWORK=/tmp/s467-stall-394.work go test ./internal/agentos -run TestStopSprintOwnerTerminatesAProvenOwnedHungManager -count=1 -v`
```
    sprint_owner_stall_unix_test.go:100: stop of a proven-owned hung manager: read unix ->/var/folders/.../s467fm3328793147/sprint-457-manager/ctl.sock: i/o timeout
--- FAIL: TestStopSprintOwnerTerminatesAProvenOwnedHungManager (3.30s)
```
GREEN (a1bcb1a3) `... -run 'SprintOwner' -count=1 -v` — new test + all 14 existing sprint-owner tests:
```
--- PASS: TestStopSprintOwnerTerminatesAProvenOwnedHungManager (3.37s)
ok  	github.com/qiangli/bashy/internal/agentos	5.591s
```
go vet clean, gofmt clean. Consequence: weave's existing recoverStalledSprintOwner (stop → relaunch) now
succeeds against a real hung manager instead of always refusing.

## Step 3 — 01:44 review: re-prove before SIGKILL and before recording stopped
Seams: `terminateGrace`, `terminateProcessStart` (vars). Fixture child may ignore SIGTERM (forces SIGKILL path).
RED `go test ./pkg/foreman -run TestTerminate -count=1 -v`
```
    terminate_unix_test.go:191: Terminate after a recycle during grace: got <nil>, want ErrServeNotProven
--- FAIL: TestTerminateReprovesBeforeSIGKILL (0.26s)
    terminate_unix_test.go:231: Terminate over a replaced session: got <nil>, want ErrServeNotProven
--- FAIL: TestTerminatePreservesASessionReplacedWhileWaiting (0.10s)
```
GREEN
```
--- PASS: TestTerminateStopsAProvenOwnedHungServe (3.08s)
--- PASS: TestTerminateRefusesAPidItCannotProve (0.14s)
--- PASS: TestTerminateReprovesBeforeSIGKILL (0.34s)
--- PASS: TestTerminatePreservesASessionReplacedWhileWaiting (0.06s)
ok  	github.com/qiangli/yoke/pkg/foreman	4.208s
```
Changed identity → ErrServeNotProven, no SIGKILL, state + socket preserved. go vet (darwin, GOOS=linux) clean.

## Step 4 — actual supervisor routing (steward registry, no new registry)
`reportSprintOwnerStall` now sends to `sprintSupervisorAddress(sprint, epic, owner)`: the HELD
`deputy:<scope>` seat covering the sprint (sprint list or epic), else the HELD `steward` seat — the board
addresses pkg/steward already registers (lexiconseat.go rolesForBoard). A seat held by the stalled owner is
skipped. None → thread entry says "no supervisor notified: no steward or deputy seat is held over sprint #N",
nothing sent; the instructing caller is NOT substituted. Read-only: never creates a seat.
RED: tests updated first (caller = instructing-caller, supervisor stub = deputy:1, + gap test) →
`vet: pkg/weave/sprint_owner_stall_test.go:176:9: undefined: sprintSupervisorAddress` (prior code sent to the caller).
GREEN `go test ./pkg/weave -run 'TestSprintInstruct(StalledManager|OrdinaryFailure|HasNoRecoverFlag)' -count=1 -v`
```
--- PASS: TestSprintInstructStalledManagerIsReportedAndRecovered (8.12s)
--- PASS: TestSprintInstructStalledManagerThatCannotStopIsNotRelaunched (3.28s)
--- PASS: TestSprintInstructOrdinaryFailureIsNotTreatedAsStall (7.01s)
--- PASS: TestSprintInstructHasNoRecoverFlag (7.64s)
--- PASS: TestSprintInstructStalledManagerWithNoSupervisorReportsTheGap (5.19s)
ok  	github.com/qiangli/yoke/pkg/weave	31.893s
```
Gap: defaultSprintSupervisorAddress has no test against a real temp steward seat yet (tests stub the var).

## Step 5 — heartbeat/progress detection without an instruct (existing immune tick)
`checkSilentSprintManagers` runs in `runQuotaImmuneTick` (so both `weave immune` and the `weave heartbeat`
loop), beside the existing `checkQuotaImmuneManagers`: a doing sprint whose holder's room card is a live
Foreman session for `sprint-N-manager` while the lease seat is not live (lapsed/unknown) → one stage entry
(`silent:` marker, deduped per lease heartbeat, claimed under queue.lock) + notice to the steward-registered
supervisor. Recovery remains the owner lifecycle (instruct → recoverStalledSprintOwner → bashy Terminate fallback).
RED (hook removed from weave_immune.go) `go test ./pkg/weave -run TestImmuneTickReportsASilentManagedManagerOnce -count=1`
```
--- FAIL: TestImmuneTickReportsASilentManagedManagerOnce (0.05s)
    sprint_owner_stall_test.go:338: actions=[] err=<nil>
```
GREEN `... -run 'TestImmuneTickReportsASilentManagedManagerOnce|TestQuotaImmune' -count=1 -v`
```
--- PASS: TestImmuneTickReportsASilentManagedManagerOnce (0.09s)
--- PASS: TestQuotaImmuneWarnCriticalGraceAndDedupe (0.27s)
--- PASS: TestQuotaImmuneForecastThresholdsAndDryRun (0.06s)
--- PASS: TestQuotaImmuneObservedWarningAndExhaustion (0.00s)
--- PASS: TestQuotaImmuneClassifiedObservations (0.22s)
--- PASS: TestQuotaImmuneConductorAlarmRecordedOnce (0.09s)
ok  	github.com/qiangli/yoke/pkg/weave	1.475s
```
Known limits: a quota-policy load error returns from the tick before this check runs; the card proof is
stubbed in the test (fixture-only, never a real session).

## Candidate
- yoke: ONE commit on 4076e8d4 (the commit carrying this file; `git log -1`), no KB.md, no WIP ancestors,
  Agent trailer. WIP history preserved locally at `refs/wip/2368-preserved` (tree-identical).
- bashy: a1bcb1a3 on 0d2af687 in /tmp/s467-stall-bashy-394 (local, not pushed); needs the yoke pin bumped
  to the yoke candidate before it builds without the out-of-tree GOWORK.
- Cross-OS: `GOOS=linux|windows go vet ./pkg/foreman ./pkg/weave` clean. Whole-package gates: manager (remote).
