---
id: c814ac55402d
kind: bug
title: weave takeover leaves the run body stuck in handoff-only mode
seq: 2028
status: todo
labels:
    - weave
created: 2026-10-18T19:26:11.162455Z
---

Observed 2026-10-18 (Sprint 446 run 6, agent-bench): after a quota-exhaustion supervised takeover (handoff-only) the run issue body is permanently rewritten to SUPERVISED TAKEOVER Mode: handoff-only. Later same-run resumes keep that top-level instruction despite weave say, so the new holder (muse-spark1.3) completed only the handoff twice and verify exited 2 with no new code. Expected: after the handoff completes, a resume restores the original task body (or an explicit resume flag does). Workaround: new weave issue linked to the same story. Fix with a regression test.
