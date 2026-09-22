---
id: 822d45eef46b
kind: feature
title: named resource holds in bashy claim
seq: 1
status: assigned
priority: p1
labels:
    - coordination
created: 2026-09-22T16:03:58.127714Z
assignee: codex-gpt5.6-sol
sprint: 251
sprint_id: 40bcd7c0-49e6-5ad5-aa21-30c2647e2e21
sprint_title: 'bashy claim: voluntary exclusive holds on shared resources'
---

Generalize the existing `bashy claim` mechanism from intersecting project path
sets to either a project or a named resource. The motivating resources are the
two shared DO test droplets.

This is one feature and one implementation seam. It absorbs the former
flock-form and manager-guidance stories; do not split those back out.

## Required behavior

- Existing project claims continue to behave unchanged.
- `bashy claim NAME --intent TEXT` takes a detached, host-local lease on NAME.
- A conflicting claim is refused and identifies the holder, intent, and age.
- Re-claim by the same holder is idempotent and refreshes the lease without
  changing its original acquisition time.
- `bashy claim release NAME` releases the named hold.
- `bashy claim list` shows project claims and current named holds, including
  mode and honest liveness. Unknown is never rendered or treated as free.
- `bashy claim NAME --wait DURATION` waits only for the bounded duration.
- `bashy claim NAME -- COMMAND` holds NAME with a kernel lock for the child
  process, propagates its exit status, and releases automatically when the
  process exits or is killed. It has no TTL or heartbeat.

The detached form retries the complete read-decide-write acquisition cycle;
the ledger mutex is not itself the hold. The attached form may use the existing
lockfile primitive because the kernel lock is the hold. No waiter registry,
push wake-up, semaphore, slot count, or new coordination package.

## Operational guidance

Add one concise rule to sprint help and the existing sprint/conductor skills:
claim a shared host before use and release it afterward. State that the hold is
advisory and host-local; it coordinates agents on this host and says nothing
about activity initiated elsewhere. Never force another holder off without
contacting them.

Append the narrow D12 decision to
`docs/agent-lock-coordination-design.md`: D11 continues to govern internal
single-process locks; a user-requested resource hold spanning agent invocations
is a different object. Register the two existing test-droplet names locally and
wrap their shared runner where practical, without recording addresses in the
repository.

## Acceptance

Focused tests prove named exclusion between distinct principals, useful
refusal, idempotent re-claim, release and reclaim, bounded wait, lapsed reclaim,
unknown-not-free, project/named non-conflict, child exit-status propagation,
cleanup after child termination, and no lock fd inherited by the child.

Run:

```sh
cd yoke
go test ./pkg/policy/coord/... ./pkg/board/...
scripts/crossvet.sh

cd ../bashy
go test ./internal/agentos
```

## Out of scope

- persistent inventory of unheld resources;
- registration metadata, `claim forget`, or a resource reference kind;
- sprint-take inventory and terminal-board trailers;
- cross-host coordination, notifications, enforcement middleware, history,
  semaphores, barriers, or capacity slots.

Story `2d182deefb31` separately projects current holds into the existing
`bashy apps` Sprint board for human users.
