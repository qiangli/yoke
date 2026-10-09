---
id: 097abd705e1c
kind: bug
title: weave start reuses a half-made workspace after a failed launch (unpopulated --no-checkout clone), so the next run starts with every tracked file deleted
seq: 33
status: todo
priority: p2
labels:
    - weave
created: 2026-10-09T09:21:00.042977Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Found 2026-10-09 by the S379 conductor: vsc-pcts-harness-kit run 2 failed at checkout -b (fcdf1dbf, now fixed) and left workspaces/issue-2 as a --no-checkout clone; the relaunch saw os.Stat(workspace) succeed, skipped clone+checkout (pkg/weave/weave_impl.go near the clone block), and the worker found 1766 tracked deletions and refused. Fix: on a failed clone/checkout weaveMarkLaunchFailed must remove the half-made workspace, or start must verify the existing workspace is on the run branch with a populated tree. Red/green test in pkg/weave.
