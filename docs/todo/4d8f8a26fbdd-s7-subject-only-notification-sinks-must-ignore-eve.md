---
id: 4d8f8a26fbdd
kind: bug
title: S7 subject-only notification sinks must ignore event body
seq: 1902
status: todo
priority: p1
labels:
    - yoke
    - skills
    - notify
created: 2026-10-11T09:48:37.289379Z
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Supervisor read-only finding on S7 at Yoke skills/agent-hub.md lines 74/110/142: desktop, ntfy, webhook ingest raw bashy events --follow JSON and currently read .body // .summary, then clip. A raw event body can contain operation/message contents; clipping is not subject-only and can leak outside host. Remove body fallback in all three generic sink functions. Use only explicit nonsecret subject/summary and event type/target coordinates; blank summary becomes safe title. Add a focused sentinel test that feeds an event with secret body but no safe summary and proves all external payload paths exclude sentinel; test valid summary still delivered. Keep existing sink/config machinery, no per-event special case. Focus skills/agent-hub.md and adjacent skill tests only; no overlap with S13 pkg/bus. Verify the focused test plus relevant Bash/Go skill packaging gate. Manager source review and final external sink proof; no false claim. Commit trailers Sprint/Story/Story-ID/Agent, private managed run no push.
