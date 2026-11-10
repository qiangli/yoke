---
id: b9432a1a1ff4
kind: bug
title: weave abandon refuses runs whose commits were merged as patch-equivalent cherry-picks
seq: 3084
status: todo
priority: p3
labels:
    - weave
created: 2026-11-10T12:01:22.694802Z
---

After merging a run's commits with git cherry-pick (different SHAs), 'weave abandon N' refuses: 'holds unmerged work'. Expected: use git cherry / patch-id equivalence (the umbrella cleanup policy) so integrated work is recognised and the workspace can be removed.
