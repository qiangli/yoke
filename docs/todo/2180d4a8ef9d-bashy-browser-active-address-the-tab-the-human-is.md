---
id: 2180d4a8ef9d
kind: enhancement
title: 'bashy browser --active: address the tab the human is looking at'
seq: 3139
status: done
priority: p2
created: 2026-11-20T13:44:13.987386Z
assignee: claude-opus5.5-s680
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: 'Human-in-the-loop browser: ping-back and remote headless web-app testing'
closed: 2026-11-20T17:46:10.72127Z
closed_by: claude-opus5.5-s680
---

Operator 2026-11-20: agents should act on the tab in focus. --active (live mode) resolves the one active tab from the tabs list and addresses the action by id; several active tabs (one per window) is an error listing them; refused outside live mode and with --url/--title/--id.
