---
id: b3941737b94c
kind: feature
title: 'M2 cast record: asciicast v2 under the platform pseudo-terminal (Unix pty + Windows ConPTY)'
seq: 3095
status: wontfix
priority: p1
labels:
    - media
    - windows
created: 2026-11-11T02:40:04.880951Z
sprint: 603
sprint_id: c808e55d-6ce2-5525-8829-96499861c90e
sprint_title: 'Agent-assisted PDF and video: small verbs, a media skill-script and kb pages any agent can follow'
closed: 2026-11-11T03:52:26.628712Z
---

Spec: dhnt-docs docs/sprint-603-master-execution-plan.md (M2). New story.

GOAL: a small core verb `cast record OUT.cast [--cols N --rows N] [--idle-limit S] -- CMD [ARGS...]` that runs CMD under the platform pseudo-terminal (Unix pty and Windows ConPTY) and writes asciicast v2 (header line `{"version":2,"width":..,"height":..,"timestamp":..,"env":{..}}` then `[seconds,"o","data"]` lines). Same output on macOS, Linux and Windows; replaces asciinema (GPL, no Windows build) as the recorder for terminal scenes in examples/video.

Reuse the existing pseudo-terminal code in yoke (pkg/agentpty, pkg/webterm; pty_windows.go) — do not add a new pty dependency. Register it beside related verbs the way other yoke cmds are registered; `--help` follows the house skill+MCP help form. Exit status = CMD's exit status. About 100 lines plus tests.

Prior art (MIT, read only, do not vendor): github.com/gvcgo/asciinema, github.com/tui-cs/tuirec (both record asciicast over ConPTY on Windows).

TEST: `go test` for the new package: record `printf '\033[31mred\033[0m\n'` and a short sleep, parse the cast, check header width/height, monotonic timestamps, the colour bytes preserved; on Windows the same test must run (no build tags that skip it). Targeted tests only, no full suite.

Agent plan: raced — several vendors each build one; the manager compares and keeps the best.

Withdrawn from core 2026-11-10 before push (operator: custom command unless core is necessary). Delivery moves to bashy story M2b (examples/cast). Code kept at refs/salvage/s603-m2-core in this checkout.
