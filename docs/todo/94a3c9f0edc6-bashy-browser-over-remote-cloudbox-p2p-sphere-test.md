---
id: 94a3c9f0edc6
kind: doc
title: 'Test web apps on a headless server from the local live browser: tunnel recipe (outpost ssh tunnel)'
seq: 3141
status: todo
priority: p3
created: 2026-11-20T13:59:39.188769Z
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: 'Human-in-the-loop browser: ping-back and remote headless web-app testing'
---

Operator 2026-11-20, narrowed: only tunnelling is needed. The browser is on the agent's host (operator desktop, live-mode extension); the web app runs on a remote headless server; the agent already drives the local browser (--active / --id).

The tunnel already exists: `outpost ssh tunnel <name> [--jump <alias>] -L <local>:<remote-host>:<remote-port>` (peer ticket from cloudbox, LAN-direct when possible, else the cloudbox-tunneled path). The server must allow local forwarding (outpost SSHAllowLocalForward).

Remaining (small): verify end to end once (app bound to loopback on a headless test host -> tunnel -> open http://localhost:<local> in the local Edge/Chrome -> agent reads text/screenshot/clicks via live mode), then add the recipe to `bashy browser --help` (login/--active area) so agents use it instead of publishing the app. No new code unless the verification finds a gap.
