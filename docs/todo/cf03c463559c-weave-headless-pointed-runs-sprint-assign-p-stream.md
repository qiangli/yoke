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
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Steward 2026-09-30, Sprint #319 (dhnt queue dhnt-31437fad): run #5 (claude sonnet5.5, points 5, launch_spec.max_runtime=1200000000000 = 20m, argv '-p --output-format stream-json') ran 12:08:35Z-12:57:30Z = 48m55s, exit 0, no delivery; run #6 (codex exec gpt-5.6-sol, points 5, same max_runtime) ran 26m28s. Both launched through 'sprint assign' (manual override). PTY-launched runs the same day were killed exactly at the cap ('[agent] terminating subagent: runtime exceeds --max-runtime 20m0s', yoke #126-128). So the ceiling is enforced only on the PTY path. Fix (KISS): enforce max_runtime in the one place every launch path shares (the run supervisor), killing the process group at the cap. Red/green: a headless pointed run of a sleeping fake agent is killed at its cap.

Conductor scope (2026-10-08): also take bashy todo e52c1c465134 (sprint wait conductor-idle fires after 5 min; runbook says 30 min). For cf03c463: headless pointed runs (sprint assign, -p stream-json, codex exec) record max_runtime but are never killed at the cap - enforce it in weave (process-group kill, run marked capped, workspace preserved), red/green test with a short cap. Do not touch pkg/fleet types or agentlaunch identity (Sprint 321).

Worker rules (Sprint 379 conductor, 2026-10-08): work in the submodule named above; commit and push INSIDE it to its default branch (public repos: main), then stop - the conductor bumps the umbrella pin. Every commit carries Sprint: #379 plus Story/Story-ID trailers for this story, with BASHY_AGENT set to your own binding. Reproduce first with a red unit test, fix at the root, never skip, quarantine or add to a known-failures list. This dev box (Dragon) is for builds and focused unit tests only. Windows runs go to noviwin1.local, Linux runs to the novidesign.local podman machine bashy or the repo CI, and macOS full suites to novidesign.local (all passwordless ssh; use the .local names). Gate before push: go build ./... and go vet on the module, plus the focused tests, with the exit code captured (never gate a push on a pipe). Report: the root cause, the commit SHA, the exact tests run and where, and any CI run URL.
