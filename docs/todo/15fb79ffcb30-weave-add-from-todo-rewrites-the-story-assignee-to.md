---
id: 15fb79ffcb30
kind: bug
title: weave add --from-todo rewrites the story assignee to the operator
seq: 3081
status: todo
priority: p1
labels:
    - weave
    - sprint
created: 2026-11-10T12:01:22.493392Z
---

Seen repeatedly in S606/S583: after 'bashy todo edit ID --owner AGENT', running 'bashy weave add --from-todo ID' set the story's assignee back to the invoking operator, so 'bashy weave start --run N -- AGENT' failed with 'claim worker story: story ID is held by <operator>'. Expected: --from-todo keeps an existing assignee (or takes --owner), and start can claim for the assignee. Workaround: re-run todo edit --owner after weave add.
