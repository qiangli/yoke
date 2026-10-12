---
id: ca9d31562ece
kind: bug
title: 'git (native, Windows): worktree remove --force declines (ErrUnsupported) and leaves the linked worktree''s commondir file locked'
seq: 1906
status: todo
labels:
    - windows
    - git
created: 2026-10-12T04:32:21.673053Z
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

Pre-existing on Windows (verified at a234655~1 and 8b2c14c on noviwin1): TestNativeWorktree_AddRemoveList fails at remove --force with ErrUnsupported, and TempDir cleanup reports .git/worktrees/wt1/commondir in use by another process - an open handle survives the native call. Find the unclosed file and why remove --force declines only on Windows.
