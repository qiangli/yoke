---
id: ebc94985bf78
kind: feature
title: S9 measured capability routing in toolcmd and selection events
seq: 1889
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T07:57:01.40741Z
weave: 86
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T08:15:10.598711Z
resolution: fixed
closed_by: codex-gpt6-sol
---

Scoped child of Sprint 418 S9 Story-ID eeebaf752cc6. Own yoke/pkg/toolcmd, pkg/llmbudget integration and event publication only. Implement bashy tool cmd run --capability deep-research -- QUESTION using the existing capability/chat provider selection, measured providers and remaining quota. Return a typed outcome containing the actual selected non-caller vendor and publish an S3 selection event; never silently seed or route an unmeasured command. Preserve S8 typed toolcmd/handoff contract. Do not edit pkg/chat/acp, pkg/fleet/baseline/tools, Bashy S16 or outpost S18; remote peer door and live two-vendor run remain manager gates on original S9. Verify GOWORK=off go test ./pkg/toolcmd/... ./pkg/llmbudget/... ./pkg/chat/... and focused selection/event tests on the private branch; manager owns Mac/Linux native and live remote gate. Commit with Sprint #418, this Story-ID and Agent trailer. Dragon short focused runs only.
