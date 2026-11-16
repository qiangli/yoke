---
id: ac46e1b3892b
kind: bug
title: browser text includes inline <script> source on some pages (Partner Center)
seq: 3125
status: todo
priority: p2
labels:
    - browser
created: 2026-11-16T22:02:57.153163Z
---

Seen 2026-11-16 in live mode (Edge 0.11.6) on partner.microsoft.com/dashboard/home: 'bashy browser text' returned the page's inline script source (obfuscated td_* JS) among the visible text. Visible-text extraction should skip script/style/noscript content. Root cause not yet identified (frame walk vs. textContent of a hidden node?).
