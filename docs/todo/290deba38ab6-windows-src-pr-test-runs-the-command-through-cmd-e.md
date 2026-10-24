---
id: 290deba38ab6
kind: bug
title: 'Windows: src pr --test runs the command through cmd.exe (so ''true'' fails) and the native git ignores GIT_AUTHOR_*/GIT_COMMITTER_* env and rejects ''commit -q'' and ''git -c'''
seq: 2041
status: todo
priority: p2
created: 2026-10-23T19:37:40.489377Z
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
---

Found 2026-10-23 in the Sprint 458 M3 Windows rerun (bashy 49a3a6f0, yoke 165e4a7f). (1) 'bashy src pr open ... --test true' recorded 'test fail (true)' on Windows; the same command passes on macOS and Linux. The PR test command should run through bashy so POSIX commands behave the same on every OS. (2) The native git engine: 'git commit -q' -> unknown shorthand flag q; 'git -c user.name=...' -> not applied; GIT_AUTHOR_NAME/EMAIL and GIT_COMMITTER_* env are ignored ('commit: author field is required'). Real git honors all of these, and agents use them for scripted commits. Expected: parity, or a clear routing to the external git.
