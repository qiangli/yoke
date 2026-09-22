---
id: 2d182deefb31
kind: feature
title: show current claims in the existing bashy apps Sprint board
seq: 4
status: assigned
priority: p2
labels:
    - coordination
created: 2026-09-22T16:06:48.57307Z
assignee: codex-gpt5.6-sol
sprint: 251
sprint_id: 40bcd7c0-49e6-5ad5-aa21-30c2647e2e21
sprint_title: 'bashy claim: voluntary exclusive holds on shared resources'
---

Human users need to see when an agent is holding a shared resource. Add a small,
read-only Claims panel to the existing `bashy apps` Sprint board after story
`822d45eef46b` supplies named holds.

## Required change

Use the board's existing panel registry and browser rendering path. Add one
`claims` panel in `yoke/pkg/board/panels.go`, shaped like the existing panels:

- title: `Claims`;
- collapsed summary: number of named resources currently held on this host;
- columns: resource, holder, state, intent, held for, and mode;
- source: the existing `coord.List` projection.

Show current holds only. The state is the honest liveness word (`live`,
`lapsed`, or `unknown`), never a boolean. Wording must make the host-local scope
clear; it must not imply the remote machine itself is idle. Unknown is never
rendered as free.

This is a change to the existing `bashy apps` Sprint board, not a new app,
route, service, API, refresh mechanism, or mutation surface. Do not add terminal
board output, release/force buttons, notifications, history, or persistent
inventory.

## Acceptance

- The panel builds for zero, live, lapsed, and unknown named holds.
- The existing Sprint-board overview JSON contains the panel.
- The existing page renders it without script or DOM failure.
- Project claims do not become misleading shared-resource rows.

Run:

```sh
cd yoke
go test ./pkg/board/...
go test ./pkg/webconsole -tags verifydom
```
