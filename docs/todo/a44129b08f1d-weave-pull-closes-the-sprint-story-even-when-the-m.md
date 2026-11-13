---
id: a44129b08f1d
kind: bug
title: weave pull closes the sprint story even when the merged run only records a blocker
seq: 3112
status: todo
priority: p2
labels:
    - weave
created: 2026-11-13T06:09:11.924656Z
---

S627 2026-11-13: 'weave pull 33' merged run #33, whose only commit recorded a BLOCKED M2b-song attempt (no artifact, story submitted as blocked), and the sprint story 0d2e5664 was marked done; conductor had to reopen it with 'todo status todo'. Expected: merging evidence should not close the story unless the conductor accepts it (or the submit says done, not blocked).
