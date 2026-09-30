---
id: 34eb1bf2cbe0
kind: bug
title: 'model door serves a stale fleet catalog: a long-running llm serve lost claude/muse, so every door-claude/door-muse request fails 409 until restart'
seq: 17
status: todo
priority: p1
labels:
    - door
created: 2026-09-30T11:24:02.941438Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Steward genie trial 2026-09-30 (yoke weave #127/#128): the door (bashy.real llm serve, up since 2026-09-29 08:33, on an older bashy build) answered every door-claude-opus5 / door-muse-spark1.3 request with 409 'sticky: identity <d>: claude is now "unknown" (was "2.1.283 (Claude Code)"); the binding cannot be served exactly'. 'bashy llm pools' listed only codex-gpt-5.5 and agy-gemini3.8-flash - claude and muse were missing from the door's catalog, so the version probe resolved nothing. The pinned binary (~/.bashy/tools/claude/2.1.283/claude) existed and reported 2.1.283; 'claude --version' and 'muse --version' answered in 0.3 s. 'bashy llm down && bashy llm up' fixed it at once (genie-opus5 answered pong). Fix (KISS): the door reloads the fleet catalog when an agent or tool lookup misses, before refusing. Red/green: a door whose catalog gains a tool after start serves it without restart.
