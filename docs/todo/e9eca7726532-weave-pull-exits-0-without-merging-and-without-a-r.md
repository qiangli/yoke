---
id: e9eca7726532
kind: bug
title: weave pull exits 0 without merging and without a reason
seq: 3080
status: todo
priority: p1
labels:
    - weave
created: 2026-11-10T12:01:22.425742Z
---

Seen during S606/S583 W0 (2026-11-09): 'bashy weave pull 63' and 'pull 63 --force' printed only the isolation warning and exited 0; main did not move and the run stayed 'submitted'. Cause on that host: the live checkout had an unstaged change (a deleted story file). Expected: refuse loudly with the reason (dirty worktree: <paths>) and a non-zero exit; never exit 0 without merging. Workaround used: git cherry-pick of the run's commits.
