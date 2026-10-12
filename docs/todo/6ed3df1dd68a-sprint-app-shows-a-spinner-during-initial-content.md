---
id: 6ed3df1dd68a
kind: bug
title: Sprint app shows a spinner during initial content loading
seq: 1998
status: done
priority: p1
created: 2026-10-12T17:24:04.288765Z
assignee: s9999-maintenance-manager
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T17:30:16.170709Z
closed_by: s9999-maintenance-manager
---

Operator request: Sprint app first load is blank while content is fetched. Show a visible accessible spinner immediately, clear it when board content arrives, and replace it with an actionable error if loading fails. Preserve existing content during background refresh. Browser verification with delayed Sprint API. Keep sprint/admin open; no unrelated changes.
