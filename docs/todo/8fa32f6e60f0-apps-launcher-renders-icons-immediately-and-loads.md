---
id: 8fa32f6e60f0
kind: bug
title: Apps launcher renders icons immediately and loads app content on demand
seq: 1997
status: assigned
priority: p1
created: 2026-10-12T16:45:14.558767Z
weave: 204
assignee: qiangli
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

Operator request: app landing page is slow, likely peer detection/sprint loading. Show app icons immediately after page load without fully loading each app; clicking an app not yet loaded shows a spinner until its page is ready. Scope: fast catalog/bootstrap, independent background status/session/look/inbox/peer updates, on-demand selected app navigation with accessible loading state and failure/retry. Preserve same/new-tab preferences, true links/modifier clicks, auth/device scopes, custom launchers and base-prefix mounts; no eager sprint/app-content requests. Verify with browser tests that delayed supplemental endpoints cannot delay tiles, selected slow app shows spinner and opens correctly, errors remain recoverable. Measure installed actual landing page. Do not close sprint/admin2005 or create unrelated stories.
