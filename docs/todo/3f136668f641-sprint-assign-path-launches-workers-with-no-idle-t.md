---
id: 3f136668f641
kind: bug
title: 'sprint assign path launches workers with no idle-timeout: an agy run sat 20 min at 0% CPU with no output until manually abandoned'
seq: 2036
status: done
priority: p2
created: 2026-10-23T17:35:03.120887Z
assignee: agy-gemini3.8-flash
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T07:49:23.829054Z
closed_by: s497-manager
---

Found 2026-10-23 Sprint 458 run dhnt#130 (agy gemini3.8-flash, --print-timeout 40m): no PTY capture, no file changes and 0% CPU for 20 min; weave list said 'working'. weave start has --idle-timeout but it defaults off and sprint assign does not set it. Expected: assign-path runs get a default output-idle timeout (e.g. 10m) and report 'stalled' instead of 'working'.

## Rework 1 (s497-manager, 2026-10-24). Run #617 rejected: over-scoped + a 161 MB `yoke` binary in the commit.
Deliver ONLY the bug fix, minimal diff:
- pkg/weave/sprint_assign.go: a default output-idle timeout (10m) for every worker launched by `sprint assign` (model and script paths), plus `--idle-timeout` on `sprint assign` to override (0 keeps the default; document). Pass it to runWeaveStart through the existing weaveStartOptions.idleTimeout, which already implements the kill. Same for sprint_reassign.go if it launches.
- Tests: sprint_assign_test.go asserts the launch carries IdleTimeout=10m by default and the flag value when given.
- Do NOT add a "stalled" state, do NOT change weave_lifecycle.go/board model/webconsole/weave list display/docs state machine, do NOT add a new watchdog (the existing --idle-timeout kill path is enough).
- Never commit build outputs: build with `go build -o /dev/null ./...` or `go vet`. `git status` before commit must show only the source files.
Gate: go test ./pkg/weave/ -run 'SprintAssign|Reassign' and go build ./... . Commit yourself with this story's trailers.
