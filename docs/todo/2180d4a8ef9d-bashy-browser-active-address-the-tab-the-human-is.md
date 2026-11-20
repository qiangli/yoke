---
id: 2180d4a8ef9d
kind: enhancement
title: 'bashy browser --active: address the tab the human is looking at'
seq: 3139
status: todo
priority: p2
created: 2026-11-20T13:44:13.987386Z
---

Operator 2026-11-20: agents should act on the tab in focus. --active (live mode) resolves the one active tab from the tabs list and addresses the action by id; several active tabs (one per window) is an error listing them; refused outside live mode and with --url/--title/--id.
