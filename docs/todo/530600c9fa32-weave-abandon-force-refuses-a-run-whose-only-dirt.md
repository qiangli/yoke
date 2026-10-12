---
id: 530600c9fa32
kind: bug
title: weave abandon --force refuses a run whose only dirt is inside a nested submodule (half-hydrated); an empty run can never be cleared
seq: 1911
status: assigned
labels:
    - weave
created: 2026-10-11T17:23:40.091726Z
assignee: s9999-workspace
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

Observed 2026-10-11, yoke run 109 (Sprint 420): allocated at 04:20 PDT, agent never launched, 0 commits; workspace setup was interrupted mid-hydration of external/podman/src (about 10k staged deletions inside the submodule; related hydration defect 1905). 'weave abandon 109 --disposition empty --force' fails: '--force could not commit 1 uncommitted file(s) for preservation, refusing to destroy them: git commit: exit status 1 ... modified: external/podman/src (modified content, untracked content)'. The parent repo cannot commit content changes inside a submodule when the gitlink itself is unchanged, so the preservation commit always fails and abandon refuses. Expected: preservation ignores submodule-internal dirt whose gitlink is unchanged (git status --ignore-submodules=dirty), or preserves it inside the submodule; a run with no commits and no parent-repo changes is abandoned and its workspace removed. Also weave list keeps showing it as allocated* with a dead wrapper. Acceptance: unit test with a workspace whose nested submodule has modified/untracked content and no gitlink change: abandon --force succeeds and records disposition empty; run 109 cleared on this host.
