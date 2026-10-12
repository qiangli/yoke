---
id: 7d1563e86ec0
kind: bug
title: 'git status (native): moved submodule pin invisible when submodule absent from .git/config; .git/info/exclude ignored'
seq: 1904
status: done
labels:
    - git
created: 2026-10-12T03:19:05.349554Z
assignee: s9999-gitparity-manager
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T10:45:29.638136Z
closed_by: s9999-gitparity-manager
---

Native git status (yoke/git nativeStatus, go-git) diverges from real git in two ways seen in the dhnt umbrella 2026-10-11: (1) a submodule whose checked-out HEAD differs from the index gitlink is not reported as M when .git/config has no submodule.NAME entry -- go-git newSubmodule marks it uninitialized and substitutes the expected hash; real git inspects the checkout regardless of config. (2) files listed in .git/info/exclude are reported as untracked. Repro: scratch superproject with a submodule advanced one commit and an excluded file; /usr/bin/git status --short vs bashy git status --short.
