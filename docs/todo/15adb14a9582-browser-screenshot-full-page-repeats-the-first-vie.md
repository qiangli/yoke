---
id: 15adb14a9582
kind: bug
title: browser screenshot --full-page repeats the first viewport instead of stitching down (Partner Center enroll form)
seq: 3126
status: todo
priority: p2
labels:
    - browser
created: 2026-11-16T22:02:57.242411Z
---

Seen 2026-11-16 in live mode (Edge 0.11.6) on partner.microsoft.com/.../enrollnow/msedgeaddons: the stitched capture repeated the top viewport twice and never reached the lower form (the page likely scrolls an inner container, not the document). Reported success, so the gap was silent. Root cause not yet identified.
