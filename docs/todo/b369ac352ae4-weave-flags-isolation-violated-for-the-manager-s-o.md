---
id: b369ac352ae4
kind: bug
title: weave flags isolation-violated for the manager's own sprint bookkeeping
seq: 3083
status: todo
priority: p2
labels:
    - weave
    - sprint
created: 2026-11-10T12:01:22.62657Z
---

'bashy sprint accept' / 'todo edit' rewrite docs/todo story files in the live checkout; any submitted run in that repo is then 'isolation-violated' and 'weave pull' refuses without --force. Expected: bookkeeping writes under docs/todo by sprint/todo verbs do not count as an isolation violation (or are recorded and excluded).
