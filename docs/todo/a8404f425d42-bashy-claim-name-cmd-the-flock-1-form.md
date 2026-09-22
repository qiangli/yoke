---
id: a8404f425d42
kind: feature
title: 'bashy claim <name> -- CMD: the flock(1) form'
seq: 2
status: todo
priority: p2
labels:
    - coordination
created: 2026-09-22T16:04:14.018209Z
sprint: 251
sprint_id: 40bcd7c0-49e6-5ad5-aa21-30c2647e2e21
sprint_title: 'bashy claim: voluntary exclusive holds on shared resources'
---

The second half of story 822d45ee, and the one the operator asked for by name: a tool similar to a unix file lock - voluntary and exclusive.

WHY A SECOND MODE
An agent is not a process. A detached hold (story 1) is a heartbeat lease because the agent works across many separate bashy invocations over hours. A RUN is the opposite: one child, minutes to an hour, and the kernel already knows when it dies. Do not put a TTL on that.

  attached   bashy claim do1 -- make test-bash
             lockfile held for the child's life, released by the OS on death.
             No TTL, no stale window, no heartbeat. flock(1) semantics.
  detached   story 1. Heartbeat lease, 30m, refreshed by re-running the claim.

WHAT TO BUILD
- attach.go in yoke/pkg/policy/coord: lockfile.Acquire, or AcquireWithin when --wait is given, on the hold file for the named resource; write the holder record marked attached; run the child; release on EVERY exit path including a signal. Propagate the child exit status.
- claim list must say WHICH mode it measured. An attached hold whose process is gone was already released by the kernel; a detached hold past its TTL is lapsed. They are different facts and the output must not blur them.
- Go's os.OpenFile already sets O_CLOEXEC, which closes the inherited-fd trap in docs/agent-lock-coordination-design.md (a spawned dev server outliving the agent and keeping the lock). ASSERT it in a test rather than assuming it.

THEN ADOPT IT
Declare the two DO test droplets on this host and wrap the remote-runner invocation, so taking the dev-box-side hold and the droplet-side leaf.lock is one act. Declaring is claim-then-release with the descriptive flags (story 822d45ee: the record outlives the hold) - there is no register verb, and do not add one here. Addresses stay in the fleet host catalog under the bashy config dir - never in a doc, per docs/bashy-posix-fleet-implementation-plan.md.

DOC
Append D12 to docs/agent-lock-coordination-design.md in the umbrella. D11 (2026-07-28) refused a generic verb on the grounds that a lock over any resource is a foot-gun and NOTHING HAD ASKED FOR IT. Both halves need answering, narrowly:
- D11 governs five INTERNAL locks over in-repo state, each held by one live process. None of them change, and the frozen vocabulary (Acquire/TryAcquire/AcquireWithin/Release/Owner/ErrHeld) stays the only platform pair.
- A droplet hold spans many separate processes over hours, so no fd survives it and flock structurally cannot express it. That is a different object, not the thing D11 refused.
- The premise expired: the operator asked, 2026-09-22.
Also amend the INDEX line so the doc no longer reads as design under review, nothing built.

TESTS
attached hold is released when the child is SIGKILLed; child exit status propagates; --wait blocks then acquires when the holder exits; no lockfile fd leaks to the child.

NOT IN SCOPE, deliberately, per the design doc's own review: no semaphore/barrier/condvar kinds, no waiter registry, no bus push wake, no ask-the-holder-to-release, no enforcement middleware. Slots and a cross-host carrier wait for a second host that actually contends.
