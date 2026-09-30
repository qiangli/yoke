---
id: 2729b0108b63
kind: bug
title: genie on door-muse makes no tool calls on tasks ('shell execution and filesystem writes are disabled for this session')
seq: 27
status: todo
priority: p2
labels:
    - genie
created: 2026-09-30T16:44:44.292277Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

After 3c5bbbb6 (pong passes), #340 conductor 2026-09-30: 'bashy genie -m door-muse-spark1.3 "run exactly: mkdir -p d && touch d/probe.txt && ls d"' made no tool call and answered 'shell execution and filesystem writes are disabled for this session' (harness: hasToolCalls false, 64 s). Muse behind cligw still speaks as a sandboxed agent instead of returning genie tool calls. Fix: the muse completion launch must expose genie's tool contract (or cligw maps Muse's tool-call output into the OpenAI tool_calls envelope). Red/green: the touch-file probe creates d/probe.txt.
