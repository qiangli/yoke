---
id: 94a3c9f0edc6
kind: feature
title: 'bashy browser over remote cloudbox / p2p sphere: test web apps from headless servers'
seq: 3141
status: todo
priority: p2
created: 2026-11-20T13:59:39.188769Z
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: 'Human-in-the-loop browser: ping-back and remote headless web-app testing'
---

Operator 2026-11-20: an agent on a headless server (no desktop, no browser) must be able to test web apps through a browser running on another host, reached over cloudbox (satellite/relay) or the p2p sphere (peer-direct). Example: agent on a Linux test server drives Chrome on the operator's desktop or a sphere peer, loads the app under test, reads text/screenshots, clicks, and reports.
Scope to design (shaping first, then code): (1) addressing a remote browser hub: bashy browser --host <peer|outpost> [--browser edge|chrome] reusing the live-mode hub protocol tunneled over the existing outpost mesh / sphere transport (no new public port); (2) which browser runs there: the human's own live browser (with consent per session, tab chosen via ping-back/--active) OR a solo headless Chrome the remote host provisions; (3) the app under test may live on the headless server: expose it to the remote browser through the mesh (port-forward) rather than publishing it; (4) auth/consent: only owner/member hosts in the same circle; every remote session is logged and visible in the extension; (5) artifacts (screenshots, text) come back to the agent over the same channel.
Acceptance: from a headless Linux test host, an agent opens a web app served on that host in a browser on another host (sphere peer or via cloudbox), reads its text and a screenshot, clicks a control, with no desktop and no public port on the test host; works with Edge and Chrome.
