---
id: b3a498e303cd
kind: feature
title: 'S529.10 browser hub logical on/off gate: bashy browser hub pause|resume|status + tray item; hub always on under outpost'
seq: 3122
status: done
priority: p1
labels:
    - browser
created: 2026-11-16T17:06:20.688602Z
assignee: claude-opus5.5-s529
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
closed: 2026-11-17T01:54:34.136186Z
closed_by: claude-opus5.5-s529
---

Operator design 2026-11-16. The browser-hub service stays running whenever outpost runs (baseline unit yoke/pkg/fleet/baseline/services/browser-hub.yaml becomes enabled by default); 'on/off' is a gate inside the hub, not process control. Paused: extension dials refused with a dedicated close code (extension stops redialing; popup says how to resume), /dispatch and copy-sheet endpoints refuse with 'paused by the user'. Default OPEN (operator decision). State persists across hub/outpost restarts. Controls: 'bashy browser hub pause|resume|status' (required path, works without tray/outpost) and an optional tray item 'Browser hub: on/off' (bashy/examples/tray) showing state; both call a loopback control endpoint authorized by a 0600 token file under BASHY_HOME. Tray rule reworded (README + kb page desktop-helpers-must-stay-read-only-toward-supervisor-run-units): the tray never controls processes; it may flip the hub gate. Also fix outpost todo 5c22dec5 (browser-hub missing from the outpost admin services list; root cause first). Repos: yoke, bashy (tray), outpost.
