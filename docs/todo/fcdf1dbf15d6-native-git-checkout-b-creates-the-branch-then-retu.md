---
id: fcdf1dbf15d6
kind: bug
title: native git checkout -b creates the branch, then returns ErrUnsupported, so the host-git fallback fails and weave start cannot launch
seq: 32
status: todo
priority: p1
labels:
    - git
    - weave
created: 2026-10-09T08:59:32.512063Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Found 2026-10-09 by the S379 conductor. weave start in vsc-pcts-harness-kit failed twice: 'fatal: a branch named agent/weave-issue-N already exists' from git checkout -b agent/weave-issue-N <sha> right after clone --local --no-hardlinks --no-checkout. Repro: clone that repo with --no-checkout, then yokegit.Exec(ctx, dir, [checkout -b agent/x <sha>]) returns 'git: not supported by the pure-Go implementation' AFTER creating refs/heads/agent/x and leaving 1766 deleted files in status; RunExternal (git/external.go) then re-runs host git, which refuses because the branch exists. The same launch works in sh. Two defects: (1) the native engine must decide support BEFORE any mutation (or roll back the ref it created), (2) find why this tree is unsupported (1259 regular 100644 + 518 100755 files, no symlinks/gitlinks/.gitattributes) and support it if cheap. Red/green: a test that a refused native checkout -b leaves no branch and the fallback succeeds; weave start on a --no-checkout clone of a repo like this launches.
