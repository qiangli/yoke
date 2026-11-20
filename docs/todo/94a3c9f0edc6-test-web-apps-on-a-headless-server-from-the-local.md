---
id: 94a3c9f0edc6
kind: doc
title: 'Test web apps on a headless server from the local live browser: tunnel recipe (outpost ssh tunnel)'
seq: 3141
status: done
priority: p3
created: 2026-11-20T13:59:39.188769Z
closed: 2026-11-20T14:05:59.763468Z
---

Operator 2026-11-20, narrowed: only tunnelling is needed. The browser is on the agent's host (operator desktop, live-mode extension); the web app runs on a remote headless server; the agent already drives the local browser (--active / --id).

The tunnel already exists: `outpost ssh tunnel <name> [--jump <alias>] -L <local>:<remote-host>:<remote-port>` (peer ticket from cloudbox, LAN-direct when possible, else the cloudbox-tunneled path). Paired peers accept the forward automatically; only a non-paired LAN destination needs SSHAllowLocalForward widened on the server.

Remaining (small): verify end to end once (app bound to loopback on a headless test host -> tunnel -> open http://localhost:<local> in the local Edge/Chrome -> agent reads text/screenshot/clicks via live mode), then add the recipe to `bashy browser --help` (login/--active area) so agents use it instead of publishing the app. No new code unless the verification finds a gap.

Closed by operator decision 2026-11-20: no new work needed — outpost already tunnels (outpost ssh tunnel -L, LAN-direct or via the satellite/cloudbox), so outpost + satellite covers testing a headless server's web app from the local live browser.
