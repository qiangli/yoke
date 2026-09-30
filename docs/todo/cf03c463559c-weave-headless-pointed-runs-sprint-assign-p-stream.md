---
id: cf03c463559c
kind: bug
title: 'weave: headless pointed runs (sprint assign, -p stream-json / codex exec) record max_runtime but are never killed at the cap'
seq: 22
status: todo
priority: p1
labels:
    - weave
created: 2026-09-30T13:04:10.311815Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Steward 2026-09-30, Sprint #319 (dhnt queue dhnt-31437fad): run #5 (claude sonnet5.5, points 5, launch_spec.max_runtime=1200000000000 = 20m, argv '-p --output-format stream-json') ran 12:08:35Z-12:57:30Z = 48m55s, exit 0, no delivery; run #6 (codex exec gpt-5.6-sol, points 5, same max_runtime) ran 26m28s. Both launched through 'sprint assign' (manual override). PTY-launched runs the same day were killed exactly at the cap ('[agent] terminating subagent: runtime exceeds --max-runtime 20m0s', yoke #126-128). So the ceiling is enforced only on the PTY path. Fix (KISS): enforce max_runtime in the one place every launch path shares (the run supervisor), killing the process group at the cap. Red/green: a headless pointed run of a sleeping fake agent is killed at its cap.
