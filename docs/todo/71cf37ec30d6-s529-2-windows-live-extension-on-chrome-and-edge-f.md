---
id: 71cf37ec30d6
kind: feature
title: 'S529.2 Windows: live extension on Chrome and Edge for Windows + Microsoft Edge Add-ons listing'
seq: 2826
status: todo
priority: p1
labels:
    - browser
    - windows
    - release
created: 2026-10-29T01:59:00.850408Z
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
---

Edge is the default browser on Windows and runs the same Chromium MV3 extension. Work: (1) verify the live mode end to end on noviwin1 (alias otter in public docs) with Chrome AND Edge: setup live (extract to a Explorer-visible folder, open chrome://extensions or edge://extensions, reveal folder, copy path), hub on 127.0.0.1, reconnect/hot-reload, S511 fixtures, S512.5 guard, the WinForms bashy ask form (5 choices, reveal, clipboard watch, Ctrl+V); (2) browser detection on Windows (Chrome vs Edge vs both; which one setup targets; --browser flag only if needed); (3) Microsoft Edge Add-ons listing (Partner Center account under DHNT Inc., listing assets, same permission justifications as S529.1, same release zip); (4) bugs found are fixed in pkg/browser with fixtures. Heavy runs on noviwin1, never Dragon. Test: fixture suite green on Windows Chrome + Edge; store install on Edge connects.
