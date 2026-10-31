---
id: 92f9dc4cf9ba
kind: bug
title: 'S526.B3d bug: Windows-native genie delegate fails ''agent CLI bashy not found'''
seq: 3028
status: todo
priority: p1
created: 2026-10-31T05:05:55.878124Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

On elephant Windows (bashy b8bd89df built natively), 'bashy.exe delegate --agent genie:dragon-door-codex-gpt6-sol ...' fails: agent CLI "bashy" not found (searched PATH incl. the candidate dir holding bashy.exe), also with PATHEXT and BASHY_TOOL_BINARY_GENIE/BASHY set. yoke pkg/chat/toolpath.go ResolveToolBinary/exec.LookPath path. Operator decision 2026-10-30: fix in S526 with red/green + live re-check on elephant Windows. Acceptance: unit test reproducing the Windows lookup (GOOS-independent where possible); live delegate PONG on elephant Windows with the candidate.
