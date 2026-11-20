---
id: 94a3c9f0edc6
kind: feature
title: 'bashy browser: test web apps on a remote headless server from the local live browser (cloudbox / p2p sphere)'
seq: 3141
status: todo
priority: p2
created: 2026-11-20T13:59:39.188769Z
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: 'Human-in-the-loop browser: ping-back and remote headless web-app testing'
---

Operator 2026-11-20. Topology: agent --> remote server --> browser, where the BROWSER IS ON THE SAME HOST AS THE AGENT (the operator's desktop, real Edge/Chrome with the bashy live-mode extension, password manager intact). The web app under test runs on a remote HEADLESS server (no desktop, no browser there).

Gap: the local browser cannot reach the app on the headless server without publishing it. Close it over the existing fabric, not a public port:
1. Reach: `bashy browser` (or a small companion verb) opens the remote app in the local browser through a tunnel over the outpost mesh / p2p sphere (peer-direct) or via cloudbox (relay) — e.g. a loopback port on the desktop forwarded to the server's port, or a mesh service URL. No new public port on the server.
2. Drive: the agent on the desktop uses live mode exactly as today (--active / --id, text, screenshot, click) on that tab; the agent may be the desktop session itself, or an agent on the headless server addressing the desktop's hub over the same tunnel (reverse direction) — decide in shaping, smallest first.
3. Consent and scope: only hosts owned by / attached to the same owner (circle rules); each tunnel is listed and closable; the extension shows which tabs an agent drives.
4. Artifacts (text, screenshots) stay on the desktop or return to the requesting agent over the same channel.

Acceptance: a web app served only on a headless Linux test host (loopback or private port, no public exposure) is opened in the operator's local Edge and Chrome via the sphere and via cloudbox; the agent reads its text, takes a screenshot and clicks a control through the live extension; closing the tunnel cuts access.
