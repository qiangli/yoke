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
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Steward genie trial 2026-09-30, yoke weave #128 (genie-opus5 via door): 32 model calls in 12 min (door log), no file written, killed at the 12-min cap. 'bashy genie session show' in the workspace: 'turns 0 committed, 0 failed' - the whole task is one turn, and a turn killed before it commits leaves nothing: no tool calls, no model text. The weave PTY log shows only genie's banner lines. Same blind spot on #126 (gpt-5.5, 20 min). Fix (KISS): record each tool call and its result as an event as it happens (the event log is append-only already), so 'genie session show' of an uncommitted/killed turn lists them. Red/green: kill a genie turn after two tool calls; session show lists both.
