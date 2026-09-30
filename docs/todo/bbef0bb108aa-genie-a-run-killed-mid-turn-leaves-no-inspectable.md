---
id: bbef0bb108aa
kind: bug
title: 'genie: a run killed mid-turn leaves no inspectable transcript (0 committed turns), so a supervisor cannot see what the model did'
seq: 20
status: todo
priority: p1
labels:
    - genie
created: 2026-09-30T11:35:47.335549Z
sprint: 340
sprint_id: 07abaf0d-4c54-57e7-b324-8c926b4c300f
sprint_title: Pure-Go m4, localedef and lp; listing view for the optional external POSIX tools
---

Steward genie trial 2026-09-30, yoke weave #128 (genie-opus5 via door): 32 model calls in 12 min (door log), no file written, killed at the 12-min cap. 'bashy genie session show' in the workspace: 'turns 0 committed, 0 failed' - the whole task is one turn, and a turn killed before it commits leaves nothing: no tool calls, no model text. The weave PTY log shows only genie's banner lines. Same blind spot on #126 (gpt-5.5, 20 min). Fix (KISS): record each tool call and its result as an event as it happens (the event log is append-only already), so 'genie session show' of an uncommitted/killed turn lists them. Red/green: kill a genie turn after two tool calls; session show lists both.
