---
id: 01a18978-7ae6-77d0-99b4-0b49ab81dfd1
seq: 13
form: page
type: gotcha
title: A service unit's expose.app with no port routes AT the primary listener
description: 'WHEN a bashy/outpost service unit fronts its real server with a translating proxy (loom/gitea + X-WEBAUTH-*, or any auth-header gateway): expose.app / expose.mesh with no ''port:'' resolve to the unit''s PRIMARY listener, so the proxy is silently bypassed and the app shows its upstream''s anonymous login page. Also: a ''serve'' verb that only starts the proxy in its detached ''start'' sibling has no proxy under a service unit.'
tags:
    - loom
    - fleet
    - service-unit
    - outpost
    - sso
status: candidate
evidence: 'S515.1 live round 5 (cloudbox docs/s515-1-live-round5.md @ 2c7d2a3); fixed in yoke story 9c3af2805701: pkg/fleet/baseline/services/loom.yaml expose.app port: proxy + loom serve --proxy-port; tests pkg/fleet/service_loom_frontproxy_test.go, external/loom/frontproxy_test.go'
source:
    tool: claude-opus5-v
    episode: ep-843d392542bc280d
    parent_episode: ep-7fac090dec611e44
created: "2026-10-29T21:02:16Z"
---

svcunit resolves an exposure's target as PortNamed(a.Port), and PortNamed("") is PrimaryPort() — the FIRST entry in ports[]. For loom that is gitea's own 31880, not the front proxy's 31881, so the matrix-tunnel app route handed requests straight to gitea: outpost's Remote-User/Remote-Email were never translated into X-WEBAUTH-*, and /app/loom/ served the anonymous Sign In page with no auto-registration. Renaming headers in app.ini cannot fix it — the translator was simply not in the path.

Two halves, both needed:
1. Name the listener: expose.app {port: proxy} AND expose.mesh [{name: git, port: proxy}]. A mesh forward terminates on loopback, so a peer handed gitea's port can set X-WEBAUTH-USER and be anyone.
2. The supervised argv must SERVE the proxy. A unit supervises ONE foreground command; 'loom start' spawned the proxy as a detached child, so under a unit there was no proxy at all. 'loom serve' now runs both (gitea on loopback + proxy on --proxy-port).

Corollary, pinned by test: with ENABLE_REVERSE_PROXY_AUTHENTICATION always on, gitea's HTTP_ADDR must be loopback-only (giteaBindAddr clamps it, even reconciling an app.ini left on 0.0.0.0) and REVERSE_PROXY_TRUSTED_PROXIES must stay 127.0.0.0/8,::1/128. On that listener an X-WEBAUTH-USER header IS a completed login.

Check when wiring any unit with a front proxy: does expose.* name the proxy listener, and does the unit's exec actually start it?
