---
id: c94adbbb931c
kind: bug
title: 'S511.5 bashy browser live: human-friendly install + zero-touch reconnect/upgrade (setup opens chrome://extensions, reveals folder, copies path; extension auto-reconnects; hub hot-reloads a stale unpacked extension)'
seq: 2819
status: done
priority: p0
labels:
    - browser
created: 2026-10-28T18:19:59.689307Z
assignee: s511-claude-manager
sprint: 511
sprint_id: 4a0e4dfe-3f34-5f82-aaad-0656bcdbddc2
sprint_title: 'bashy browser live mode: drive real-world forms (iframes, custom dropdowns, controlled inputs, precise click targets)'
closed: 2026-10-28T19:39:37.47515Z
closed_by: s511-claude-manager
---

Operator 2026-10-28. setup live: extract, open chrome://extensions in Chrome, reveal the folder in Finder/Explorer for drag-and-drop, copy the path, print 3 numbered steps; if an extension is already connected, refresh files and hot-reload instead. Extension: remember the first Connect and auto-reconnect on Chrome start / hub restart (loopback only). Hub: on a stale _hello send 'reload' (chrome.runtime.reload) so an unpacked extension picks up the refreshed folder with no manual reload. Fix the stale README (ycode paths). The remaining one-time human step (Developer mode + Load unpacked) disappears only with a Chrome Web Store listing (v1.0.x, prepared by S512.3). Test: Go unit tests for setup/hub reload path; live check on Dragon Chrome.

WORKER CONSTRAINTS (manager s511-claude-manager): repo yoke only. Files: cmds/browser/live.go (+ its test), pkg/browser/live/{server.go,service.go,protocol.go} for the hub 'reload' path, pkg/browser/live/extension/{README.md,popup.js} and in background.js ONLY the connection region (openWS/scheduleReconnect/onMessage/_hello, top ~460 lines) plus 'reload' in SUPPORTED_METHODS and one dispatch() branch. Do NOT touch extractInTab/runInTab/injectOne or manifest.json version (manager bumps to 0.8.0; S511.1-.4 edit those functions concurrently). Open chrome://extensions via the OS open command for Chrome (macOS: open -a 'Google Chrome' chrome://extensions; Windows: start chrome chrome://extensions; Linux: google-chrome/chromium) best-effort, never fail setup on it; clipboard copy best-effort (pbcopy/clip/wl-copy|xclip). Tests: go test ./cmds/browser ./pkg/browser/live (short; inject the opener/clipboard as funcs so tests never launch Chrome). No hostnames in code/comments. Commit trailers Sprint: 511 / Story: S511.5 / Story-ID: c94adbbb931c.
