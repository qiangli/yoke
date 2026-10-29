---
id: 9c3af2805701
kind: task
title: 'S515.1c yoke fix: satellite loom app must go through loom''s front proxy (proxy-header login) and gitea must listen on localhost only'
seq: 2935
status: done
priority: p0
created: 2026-10-29T20:39:34.360525Z
weave: 724
assignee: claude-opus5
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
closed: 2026-10-29T21:28:15.829177Z
resolution: fixed
closed_by: claude-opus5-v
---

Bug found by S515.1 live round 5 (cloudbox docs/s515-1-live-round5.md @ 2c7d2a3, branch agent/weave-issue-198): on bashy rc.22-dev the satellite-local outpost's loom app routes straight to gitea :31880, so outpost's Remote-User/Remote-Email never become X-WEBAUTH-* and /app/loom/ shows the anonymous Sign In page (no auto-registration). yoke already HAS the translating front proxy (external/loom/loom.go ~1056 stripWebauthHeaders + X-WEBAUTH-*, since 2854c03c): do NOT just rename header names in app.ini. Root-cause why the app registration/port bypasses the front proxy (suspect the S515.1b unprivileged RUN_USER=loom path). Fix: the outpost app points at the front proxy; gitea HTTP_ADDR bound to 127.0.0.1 only (direct :31880 with reverse-proxy auth on = header spoofing); REVERSE_PROXY_TRUSTED_PROXIES = loopback. Red/green unit test FIRST (front proxy in path; gitea not reachable on a non-loopback address; spoofed X-WEBAUTH-* from a client is stripped). Ships in the next bashy prerelease (operator go pending); live re-proof on sat-3a3cc323315b after that. Heavy builds on elephant, not Dragon. No host patching.
