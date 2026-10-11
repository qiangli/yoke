---
id: 6b4b248e2e3f
kind: feature
title: S6 yoke meet DM CLI and sender pairing front door
seq: 1887
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T07:36:04.003302Z
weave: 84
assignee: claude-opus5.5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T23:22:15.619653Z
closed_by: claude-opus5.5
---

Scoped child of Sprint 418 S6 Story-ID d4d28b59d4a3. Own yoke/pkg/meet relay DM CLI/front door, pairing integration and focused tests only; do not edit skills/agent-hub.md (S7 owns it) or meet/service.go (S17 owns it). Reuse POST /api/dms/AGENT/messages and /work chat.Invoke transcript path and existing app-pair scoped, expiring, single-use ticket machinery. Add bashy meet dm AGENT --channel C --sender S [--thread T] with argv text or JSON stdin, stdout reply, unknown-sender pair code with no agent launch, scoped/revocable allowlist, S3 room event per message, and S4 /approve and /deny queue resolution; do not revive retired relay verb or add second message log. Gate: GOWORK=off go test ./pkg/meet/... ./pkg/webconsole/... ./pkg/ask/... and process-level unpaired -> pair -> same message/reply, approval decision, event evidence. Manager owns native gate and S6 aggregate skills/gateway acceptance. Commit with Sprint #418, this Story-ID and Agent trailer. <door-host> short tests only.

Manager continuation at 08:00 UTC: the worker exited while waiting on a background gate, but its committed source was recovered through weave salvage and integrated. Manager inspection of `pkg/meet/dm_frontdoor.go` and `GOWORK=off go test ./pkg/meet/... ./pkg/ask/...` passed. The Mac native gate is running. This child stays blocked pending `pkg/webconsole` gate, process-level unpaired/pair/approve/reply/event transcript, and checking cross-process sender grant writes. Automatic merge status is not full acceptance; original S6 stays open.
