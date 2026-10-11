---
id: 9ba6d26cfbb1
kind: feature
title: 'S3 bashy events: expose the room timeline as the shared event log; feed sprint/handoff/todo; SSE'
seq: 1865
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T03:51:47.021854Z
weave: 73
assignee: claude-sonnet5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T06:55:55.16268Z
resolution: fixed
closed_by: claude-sonnet5.5-u
---

Reinspection 2026-10-11 (docs/agent-ecosystem-synergy.md section Reuse map): extend existing machinery, no second log or verb. The room timeline (yoke/pkg/room room.go, archive.go, timeline_reader.go; ~/.bashy/room/timeline.jsonl) already is the host log: global monotonic Seq stable across 7-day rotation, generation change = rebase. bashy events = thin verb over room.Timeline/TimelineReader reusing bus watch runFollow (--since/--follow/--kind/--json); today bus watch matches only notify (yoke/pkg/bus/watch.go). Emit resync when since precedes archivedThrough or generation changes. Producers: weaveStoryAppend (yoke/pkg/weave/weave_story.go, THE ONE HOOK) emits every thread kind, not only stage; handoff create/claim/resume (yoke/pkg/handoff, emits nothing today) and todo status changes emit; new Type constants approval, usage, inbound. SSE GET /api/events?from=SEQ on meet serve copying the yoke/pkg/dag/serve.go SSE loop. Never add a second log under BASHY_HOME. Acceptance: sprint comment, handoff and todo done each appear in bashy events --json with contiguous Seq; SSE reconnect with from=N yields no gap; resync emitted past retention.
