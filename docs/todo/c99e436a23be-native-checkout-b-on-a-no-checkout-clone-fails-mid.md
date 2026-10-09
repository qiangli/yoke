---
id: c99e436a23be
kind: bug
title: native checkout -b on a --no-checkout clone fails mid-reset and leaves a half-written index, so the host-git fallback checks out with staged deletions
seq: 34
status: done
priority: p1
labels:
    - git
    - weave
created: 2026-10-09T09:23:13.107287Z
assignee: agy-gemini3.7-flash
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-09T09:44:11.397765Z
closed_by: claude-opus5.5
---

Follow-up to fcdf1dbf (yoke 9677a49 fixed ref rollback only). Repro 2026-10-09: git clone --local --no-hardlinks --no-checkout of vsc-pcts-harness-kit (1777 files, nested dirs, a .tgz) then yokegit.Exec checkout -b agent/x <sha> returns 'worktree checkout failed: file not found' after writing an index of 832 entries; RunExternal host git then switches branch keeping 1766 staged deletions (an unborn index would have been populated). Weave run vsc#3 started on that tree. Fix: when the index is unborn (no .git/index), return ErrUnsupported before any write so host git does the initial checkout; and on any later native failure restore the original index file. Also find the 'file not found' cause. Red test: real nested tree with subdirectories cloned --no-checkout.
