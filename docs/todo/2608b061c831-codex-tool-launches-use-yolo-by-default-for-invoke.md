---
id: 2608b061c831
kind: bug
title: Codex tool launches use YOLO by default for invoke and chat
seq: 29
status: todo
priority: p1
labels:
    - codex
created: 2026-10-01T02:20:14.191817Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Operator instruction 2026-09-30: Bashy must launch Codex with the CLI approval-and-sandbox bypass for ordinary invoke and chat starts. Centralize at agentlaunch so named bindings, headless invoke, interactive chat, and steer receive the same flag. Preserve explicit read-only review and explicit safe sandbox overrides. Acceptance: focused argv tests show the bypass exactly once for ordinary Codex launches, no bypass for read-only or explicit safe sandbox, and no change to other tools; relevant Go tests pass.
