---
id: 92f9dc4cf9ba
kind: bug
title: 'S526.B3d bug: Windows-native genie delegate fails ''agent CLI bashy not found'''
seq: 3028
status: todo
priority: p1
created: 2026-10-31T05:05:55.878124Z
weave: 754
assignee: claude-sonnet5.5
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Operator decision 2026-10-30: fix in S526, red/green + live re-check on elephant Windows. Merged so far: round 1 yoke 30b0adc8 (agent CLI lookup: POSIX PATH list + running binary), round 2 yoke a38244a8 (pkg/nativepath: normalize PATH at process start). bashy main f022350e pins them. ROUND 3 (live, you own the re-check): reproduce on elephant Windows yourself: '~/bin/outpost ssh liqiang@elephant.local -- bashy -s < script.sh' (Windows bashy shell; send scripts on stdin; never put '>' in remote args). Candidate: C:/Users/liqia/s526-op/cand3/bashy.exe (bashy f022350e); rebuild there with 'cd C:/Users/liqia/s526-op/bashy; bashy git pull; bashy self build C:/Users/liqia/s526-op/candN/bashy.exe' (writes to that Windows path). Observed facts: (1) in that ssh session $PATH is EMPTY in the shell and its children; (2) with PATH exported Windows-style 'C:\...\cand3;C:\Users\liqia\AppData\Local\outpost;C:\Windows\System32' and PATHEXT set, run from C:/Users/liqia/s526-op/work: 'bashy.exe delegate --agent genie:dragon-door-codex-gpt6-sol "Reply PONG"' -> 'bashy genie: llm env: mesh forward to dragon: outpost mesh consume ls: exec: "outpost": executable file not found in %PATH%' (once it got further and hit 'runtime.controlRoot overlaps model-visible root' when cwd was HOME). Find why exec of outpost misses PATH (env passed to the genie dag child? bashy shell export to Windows children?), fix at the root with a GOOS-independent unit test, and show PONG live. NO podman/docker/make test on Dragon; focused go test only.
