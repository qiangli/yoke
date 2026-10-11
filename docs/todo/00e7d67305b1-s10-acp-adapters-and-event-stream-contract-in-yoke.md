---
id: 00e7d67305b1
kind: feature
title: S10 ACP adapters and event stream contract in yoke
seq: 1890
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T07:57:28.650992Z
weave: 88
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T08:21:04.082606Z
resolution: fixed
closed_by: codex-gpt6-sol
---

Scoped child of Sprint 418 S10 Story-ID abc4ce65c606. Own yoke/pkg/chat/acp.go, ACP adapter client tests and pkg/fleet/baseline/tools/{claude,codex,gemini} definitions only. Add pinned acp_exec adapter declarations with source, release, sha256 and availability checks; map tool_call, plan, thought and usage updates to the S3 entry event contract without dropping them, preserving typed permission requests and principal/session identity for the existing S4 ask approval queue. Publish an ACP-registry manifest as required by the original story. Do not edit pkg/toolcmd (S9) or Bashy S16/S4 bridge, outpost or live Zed/Codync. Verify GOWORK=off go test ./pkg/chat/... ./pkg/fleet/... and focused adapter goldens on your branch; manager owns Mac/Linux and real Zed/Codync/approve gates before original S10 acceptance. Commit with Sprint #418, this Story-ID and Agent trailer; short Dragon tests only.
