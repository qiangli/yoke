---
id: f970cbf46fa6
kind: feature
title: 'L1 Declared vendor session commands (clear/exit/resume/rename/compact/status) on a live session: bashy chat command'
seq: 3147
status: done
priority: p1
created: 2026-11-20T19:06:12.681843Z
assignee: agy-gemini4-carbon
sprint: 681
sprint_id: e926de3d-b59d-54e0-94aa-a40d41907a3e
sprint_title: 'Live-session vendor commands: declared clear/exit/resume/rename/compact/status'
closed: 2026-11-20T20:46:08.581516Z
closed_by: claude-opus5.5
---

Spec: umbrella docs/plan-s681-live-session-commands.md §L1 (operator request 2026-11-20: drive a controlled agent's own session commands without restarting it; declared in config like /plan so the manager/supervisor can detect and use them). Builds on fleet ToolCommand/commands: and CanonicalToolCommands; adds clear+rename, measured per-vendor seed declarations (never guessed), bashy chat command <session> <name> (verbatim, no inbox prepend, ready-wait, timeline event, refusal lists declared), sessions --json discovery, Session.Quit uses declared exit. Unit tests with the fake tui session; live smoke by the conductor only.
