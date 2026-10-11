---
id: 1a6263fdb967
kind: bug
title: S15 internal unit capability switch and status synchronization
seq: 1896
status: assigned
priority: p1
labels:
    - yoke
created: 2026-10-11T08:49:25.355701Z
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Fix pkg/service/internal.go: Configure must return restart-required when capability or owner changes; Activate after deactivation must bind the new capability; Deactivate and Health must snapshot state/capability under lock. Add meaningful capability switch/deactivate/health and concurrent status tests. Preserve existing service provider contract; verify go test ./pkg/service/... and Linux race gate. Manager-owned focused fix for Sprint418 S15 integration.
