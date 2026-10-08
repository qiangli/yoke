---
id: 2729b0108b63
kind: bug
title: genie on door-muse makes no tool calls on tasks ('shell execution and filesystem writes are disabled for this session')
seq: 27
status: done
priority: p2
labels:
    - genie
created: 2026-09-30T16:44:44.292277Z
assignee: claude-opus5.5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-08T19:46:02.920017Z
closed_by: claude-opus5.5
---

After 3c5bbbb6 (pong passes), #340 conductor 2026-09-30: 'bashy genie -m door-muse-spark1.3 "run exactly: mkdir -p d && touch d/probe.txt && ls d"' made no tool call and answered 'shell execution and filesystem writes are disabled for this session' (harness: hasToolCalls false, 64 s). Muse behind cligw still speaks as a sandboxed agent instead of returning genie tool calls. Fix: the muse completion launch must expose genie's tool contract (or cligw maps Muse's tool-call output into the OpenAI tool_calls envelope). Red/green: the touch-file probe creates d/probe.txt.
