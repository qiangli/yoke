---
id: 03f39a2a502a
kind: bug
title: a live weave run was SIGTERMed (killed_by context cancelled, exit 143) while the conductor abandoned a sibling run and ran pkg/weave tests in the same checkout
seq: 36
status: todo
priority: p1
labels:
    - weave
created: 2026-10-09T15:12:04.489732Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

S379 2026-10-09: yoke weave run 17 (claude-sonnet5.5, launched 07:58:41 PDT) was killed at 08:10:38 PDT (queue: state killed, exit_code 143, killed_by context cancelled) while working normally. In the same minutes the conductor (a) ran go test -count=1 ./pkg/weave (285 s, finished about 08:10) in the canonical yoke checkout whose weave queue (yoke-d9ca9236) held run 17, and (b) ran weave abandon 14 --yes --disposition superseded in that queue at about 08:10:3x. Other repos' live runs (ycode 3, bashy 12/13) were untouched. Find which one cancels a sibling wrapper's context: the abandon/teardown guarded cleanup (does it signal wrappers of other issues, e.g. by process pattern or queue-wide reconcile?) or pkg/weave tests touching the real ~/.bashy/weave state (tests must use a temp HOME/BASHY_HOME). Red/green test: abandoning run N never signals run M's wrapper; pkg/weave tests never touch the real weave dir.


## Investigation and regression coverage

Two independently reproduced isolation defects:

- `weaveStopWrapper` selected one recorded PID but sent TERM and KILL to
  **negative PID**, signalling its whole process group. Session creation is
  best effort and skipped for interactive launchers; group membership is not
  ownership of a run. The regression creates two test-owned wrapper processes
  in one private group and abandons only the first. Before the fix the sibling
  dies; after the fix only the selected wrapper exits. Wrapper cancellation
  still owns stopping its separately recorded child group.
- Package `TestMain` replaced HOME/USERPROFILE but retained BASHY_HOME and
  BASHY_*_DIR overrides. In particular, sprint-store overrides outrank HOME
  and can expose an operator board containing live run links. A self-exec
  regression injects disposable external store paths and proves that TestMain
  clears them. Defaults follow each fixture's HOME; pinning BASHY_HOME to one
  suite-wide directory would instead leak sprint state between fixtures.

Audit: abandon finds the selected item under the queue lock. Its finalization
recovery updates expired records without signalling. The shared
`weavePruneOwnedRun` teardown reconciles only the selected item, checks its
lifecycle/settlement/artifact ownership and removes its artifacts; neither
that teardown nor sprint-end cleanup uses process-name matching. The unsafe
broadcast was inside the wrapper-stop helper before guarded teardown.

These regressions establish reachable failure mechanisms, not retrospective
proof of the original signal sender. Run 17 has since restarted and its current
queue record no longer contains the reported death. The original process-group
membership and test environment were not preserved in the story evidence.
No live worker was signalled during this investigation. Focused tests use a
private HOME/BASHY_HOME, scrub inherited BASHY_/WEAVE_ settings at invocation,
and launch only test-owned processes for signalling assertions.
