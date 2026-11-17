---
id: 94fa19ef0872
kind: spike
title: 'S529.3 Safari spike: feasibility + capability matrix for a Safari Web Extension of the live extension'
seq: 2827
status: todo
priority: p1
labels:
    - browser
    - macos
    - safari
created: 2026-10-29T01:59:00.974093Z
sprint: 651
sprint_id: 9f9ec0c8-1df1-518f-9843-991ce50f6ee4
sprint_title: 'bashy live extension: store distribution + cross-browser (Chrome Web Store, Edge Add-ons/Windows, Safari, one release target)'
---

Decide before building. Safari ships extensions only inside a signed macOS app (xcrun safari-web-extension-converter -> Xcode project; Apple Developer Program; App Store or notarized distribution — signing is blocked on the Apple D-U-N-S item, see S504). Measure on Dragon with the converter's report + a dev-signed local build: which APIs used by background.js/page.js exist in Safari (no chrome.debugger is expected -> no trusted keystrokes, no background-tab CDP capture, no network/console/perf; check scripting.executeScript allFrames/frameIds, webNavigation, tabs, cookies, clipboard, alarms/keepalive, loopback ws:// from the extension), what degrades vs what breaks, whether the guard (page.js/guard.js) works unchanged. Output: docs capability matrix (Chrome vs Edge vs Safari per bashy browser verb), a go/no-go with effort, and the build-story list. No App Store submission in this story.

Port (operator 2026-11-16): a Safari build dials its own hub on 58100, reserved as live.SafariPort (`bashy browser --browser safari`), an exception to the 58081 + first-letter rule.
