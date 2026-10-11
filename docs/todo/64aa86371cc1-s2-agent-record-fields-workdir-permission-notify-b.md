---
id: 64aa86371cc1
kind: feature
title: 'S2 Agent record fields: workdir, permission, notify, bindings, avatar'
seq: 1864
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T03:51:44.636764Z
weave: 75
assignee: codex-gpt5.6-terra
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T06:50:37.574825Z
resolution: fixed
closed_by: codex-gpt5.6-terra-w
---

Add additive fields to fleet Agent (yoke/pkg/fleet/types.go): workdir; permission auto|ask|review|deny (default auto); notify; bindings (channel+sender to agent, most-specific-wins peer then channel then default, after OpenClaw); avatar. Shape them so the Sprint 381 S8 agent fence body reuses them unchanged. Acceptance: agent add/set/show carry the fields; chat and delegate default --cwd to workdir; old records load unchanged. Spec docs/agent-ecosystem-synergy.md.
