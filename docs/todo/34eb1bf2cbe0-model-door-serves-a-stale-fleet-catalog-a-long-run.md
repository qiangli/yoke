---
id: 34eb1bf2cbe0
kind: bug
title: 'model door serves a stale fleet catalog: a long-running llm serve lost claude/muse, so every door-claude/door-muse request fails 409 until restart'
seq: 17
status: done
priority: p1
labels:
    - door
created: 2026-09-30T11:24:02.941438Z
assignee: claude-opus5.5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-08T19:16:14.082672Z
closed_by: claude-opus5.5
---

Steward genie trial 2026-09-30 (yoke weave #127/#128): the door (bashy.real llm serve, up since 2026-09-29 08:33, on an older bashy build) answered every door-claude-opus5 / door-muse-spark1.3 request with 409 'sticky: identity <d>: claude is now "unknown" (was "2.1.283 (Claude Code)"); the binding cannot be served exactly'. 'bashy llm pools' listed only codex-gpt-5.5 and agy-gemini3.8-flash - claude and muse were missing from the door's catalog, so the version probe resolved nothing. The pinned binary (~/.bashy/tools/claude/2.1.283/claude) existed and reported 2.1.283; 'claude --version' and 'muse --version' answered in 0.3 s. 'bashy llm down && bashy llm up' fixed it at once (genie-opus5 answered pong). Fix (KISS): the door reloads the fleet catalog when an agent or tool lookup misses, before refusing. Red/green: a door whose catalog gains a tool after start serves it without restart.
