---
id: 92f9dc4cf9ba
kind: bug
title: 'S526.B3d bug: Windows-native genie delegate fails ''agent CLI bashy not found'''
seq: 3028
status: todo
priority: p1
created: 2026-10-31T05:05:55.878124Z
assignee: claude-sonnet5.5
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Operator decision 2026-10-30: fix in S526, red/green + live re-check on elephant Windows. Round 1 (yoke 30b0adc8, lookInPathList + lookSelf) fixed 'agent CLI bashy not found'. Live re-check on elephant Windows (candidate bashy 7e38a14f at C:/Users/liqia/s526-op/cand2/bashy.exe, run from a bashy shell over outpost ssh, cwd C:/Users/liqia): genie now starts but fails 'llm env: mesh forward to dragon: outpost mesh consume ls: exec: "outpost": executable file not found in %PATH%'. outpost.exe is in C:\Users\liqia\AppData\Local\outpost, which is on PATH, so this is the same root cause: in-process exec.Command/LookPath on Windows gets the POSIX-spelled PATH the bashy shell exports. Round 2: fix at the root (in-process child exec on Windows sees a native PATH, e.g. normalize PATH to native at the boundary where bashy launches Go-side children / or resolve through the same helper), not per call site. Acceptance: unit test (GOOS-independent) for the PATH normalization; live: candidate 'bashy.exe delegate --agent genie:dragon-door-codex-gpt6-sol "Reply PONG"' on elephant Windows prints PONG. NO podman/docker/make test on Dragon; focused go test only. Manager does the live re-check.
