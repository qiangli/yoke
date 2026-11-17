---
id: 38d00db1bf7b
kind: feature
title: 'S529.12 companion views: bashy browser beside — any agent-made format opened beside the original page'
seq: 3124
status: todo
priority: p1
labels:
    - browser
created: 2026-11-16T18:26:17.594836Z
sprint: 650
sprint_id: b499271e-f373-5b6c-94e9-4f6c1130607d
sprint_title: 'Consumer content shaping: private POC + provisional #4'
---

Operator 2026-11-16: many tasks want a different format next to the original content: a summary, a visual poster-style digest of long prose, a translation, notes, a comparison table, a prefilled form (copy sheets are the first case). MVP verb: 'bashy browser beside FILE.html' — the agent writes a self-contained page; bashy copies it into the sheet dir, the live hub serves it on a token URL and open_beside puts it in Chrome split view next to the driven tab (beside/file fallbacks as for copysheet). --json {path, opened_as}. Reuses yoke 8cc18c9c (hub /sheet, extension open_beside). Later: .md input rendered to themed HTML; live refresh while the agent updates the view.

Showcase (operator 2026-11-16): the operator's own public résumé site as the consumer-side demo of shaping: the original page with a companion view beside it (poster-style one-page digest, recruiter summary, skills timeline). URL kept in private notes.
