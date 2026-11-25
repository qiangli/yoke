---
id: 71cf37ec30d6
kind: feature
title: 'S529.2 Chrome and Edge on Windows and macOS: live extension verified + Microsoft Edge Add-ons listing'
seq: 2826
status: todo
priority: p1
labels:
    - browser
    - windows
    - release
created: 2026-10-29T01:59:00.850408Z
sprint: 651
sprint_id: 9f9ec0c8-1df1-518f-9843-991ce50f6ee4
sprint_title: 'bashy live extension: store distribution + cross-browser (Chrome Web Store, Edge Add-ons/Windows, Safari, one release target)'
---

Edge is the default browser on Windows and runs the same Chromium MV3 extension. Work: (1) verify the live mode end to end on the Windows host with Chrome AND Edge: setup live (extract to a Explorer-visible folder, open chrome://extensions or edge://extensions, reveal folder, copy path), hub on 127.0.0.1, reconnect/hot-reload, S511 fixtures, S512.5 guard, the WinForms bashy ask form (5 choices, reveal, clipboard watch, Ctrl+V); (2) browser detection on Windows (Chrome vs Edge vs both; which one setup targets; --browser flag only if needed); (3) Microsoft Edge Add-ons listing (Partner Center account under DHNT Inc., listing assets, same permission justifications as S529.1, same release zip); (4) bugs found are fixed in pkg/browser with fixtures. Heavy runs on the Windows host and the macOS test host, never the dev box. Test: fixture suite green on Windows Chrome + Edge; store install on Edge connects.

Scope 2026-11-25 (operator): Sprint 651 supports Chromium only — Chrome and Edge on Windows and macOS. Safari (S529.3/S529.4) and per-store packaging (S529.5) are parked off the sprint. This story adds Edge on macOS to the verification matrix (Chrome and Edge, Windows and macOS); the Edge Add-ons item ships the same Chromium ZIP as the Chrome Web Store.
