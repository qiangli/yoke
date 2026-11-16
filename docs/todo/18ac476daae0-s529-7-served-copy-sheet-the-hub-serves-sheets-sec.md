---
id: 18ac476daae0
kind: feature
title: 'S529.7 served copy sheet: the hub serves sheets; secret fields by reference (reveal/copy), never in the file or the agent'
seq: 3119
status: todo
priority: p1
labels:
    - browser
created: 2026-11-16T17:06:20.187157Z
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
---

Operator design 2026-11-16 (follows S529.6). The live hub (always-on browser-hub service, or 'bashy browser hub') serves copy sheets on loopback. A field may name a secret instead of a value ({label, secret: NAME}); the sheet shows a mask with an eye (reveal ~10 s) and Copy; on click the page asks the hub, which resolves NAME from the local-only store (S529.8) and returns it to the page. The value flows store -> page -> clipboard; it never reaches the agent, the HTML file, stdout or logs. Copy clears the clipboard after ~30 s if unchanged (guard rule). Encryption-in-file was rejected: the key would ship with the page. Security: per-sheet random token, short TTL, Origin/Host checks, loopback only; write a short threat-model note before code. Refused while the hub gate is paused (S529.10). Acceptance: unit + e2e (reveal/copy round trip, token/TTL/origin refusals, value absent from file/logs).
