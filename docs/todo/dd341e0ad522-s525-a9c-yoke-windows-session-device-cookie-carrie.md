---
id: dd341e0ad522
kind: bug
title: 'S525.A9c yoke: Windows session/device cookie carries DOMAIN\user, invalid cookie byte, so every paired request is 403'
seq: 2921
status: todo
priority: p0
labels:
    - windows
created: 2026-10-29T17:03:17.641775Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Sprint 525, goal a9 (parent eb1be776). OS divergence (operator: any OS difference is a bug), found by the live A9 smoke on native Windows (installed candidate bashy 84be5506 / yoke c9e58916). The identical scenario passes pairing on Linux.

Symptom: every request from a paired device is 403 on native Windows. `bashy app pair --principal alice --allow notes` then redeeming the ticket sets the bashy_console cookie to
  ELEPHANT\liqiang#dev:<id>|<expiry>|<mac>
The value carries the raw Windows account name DOMAIN\user, and a backslash is not a valid cookie-value byte. Go's net/http logs "invalid byte '\\' in Cookie.Value; dropping invalid bytes" and drops it, so the subject no longer matches its MAC: alice and bob get 403 on /api/shape/<app>, /shape/<app>/ and the app routes. The operator login session (os:<owner>) is probably affected the same way.

Fix: encode the session/device subject in the cookie value so it only uses valid cookie-value bytes on every OS (e.g. base64url of the subject, or a server-side session id), keep the MAC over the decoded subject, and keep principals unchanged (os:<owner>, device:<principal>). It must be backward-safe: an old cookie fails closed (re-pair or log in again), never upgrades privileges. Same behaviour on darwin/linux/windows.

Acceptance: a focused test that mints and redeems a device pairing (and an operator session) with an owner name containing a backslash (e.g. "HOST\\user", injected the way the code resolves the owner). The cookie round-trips through net/http and the request is identified as device:alice / os:<owner>. Plus tampered and old-format cookies refuse. go test ./pkg/webconsole -run 'TestPair|TestShaped|TestScopeSegment|TestLogin' green; GOOS=windows go vet ./pkg/webconsole. Touch only the cookie encode/decode in the session/pairing code; do not change scope rules (S525.A9b owns scope.go in parallel). Sprint/Story/Story-ID trailers; no hostnames in code, tests use synthetic names.
