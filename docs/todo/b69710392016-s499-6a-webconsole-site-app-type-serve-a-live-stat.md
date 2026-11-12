---
id: b69710392016
kind: feature
title: 'S499.6A webconsole: ''site'' app type — serve a live static directory in-process (bashy app add NAME --type site --source DIR)'
seq: 3103
status: todo
priority: p1
labels:
    - app
    - web
created: 2026-11-12T17:05:34.390769Z
sprint: 499
sprint_id: c3c45f32-63b5-5575-980f-299a44a561f3
sprint_title: 'Content creation with bashy: YC videos and a local-first author-to-reader showcase'
---

S499.6 slice A. bashy serves a static site itself, so examples need no separate server binary (examples/vlog drops its server/ module and its Windows cross-build).

Add a third registered app type, `site`, next to `proxy` and `shaped`:
- Command: `bashy app add NAME --type site --source DIR`.
- Record: in yoke/pkg/fleet/apps.go (App.Type, validation at the type switch). A site app needs an absolute existing --source directory and has no port, no shaping, and no start argv. Update the Type/Source doc strings, `app schema`, `app show`, and `app set` validation.
- LINKED, never copied: unlike shaped import (fleet/shaped_import.go), the record points at the live directory. An edit on disk is served on the next request, with no re-register.
- Serving: in-process in yoke/pkg/webconsole, mounted at /<name>/ through the existing registered-panel path (registered_http.go currentPanels, coopauth.Mount, X-Forwarded-Prefix). Same auth tiers as other registered apps (system default; public and custom as today).
  - `/` serves index.html. Directory listings are off.
  - http.ServeContent semantics, so Range requests work (video seeking), with correct MIME types, ETag and Last-Modified.
  - Refuse any path segment starting with '.' (.git, .bashy, .env …) and anything matching the source's .gitignore if one exists.
  - Resolve symlinks and refuse anything outside --source (no escape).
  - Headers: X-Content-Type-Options: nosniff, and `Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups allow-downloads`. There is NO allow-same-origin, so site scripts run in an opaque origin and can never read console cookies or call console APIs.
  - The site's own content is served unmodified: no suite chrome injected.
- Same on every OS: Windows paths and drive letters; case-insensitive filesystems must not open a bypass of the dot-segment rule.

Tests (targeted unit tests only; no full or remote gates in the coding session):
- record validation: missing or relative source, port given, unknown type
- index.html served
- traversal (../, encoded %2e%2e, backslash on Windows)
- dot segment refused
- symlink escape refused
- Range request returns 206 with the right bytes
- CSP and nosniff headers present
- a live edit is visible on the next request
- the prefix-mounted links work
Cross-build `GOOS=windows`.

Out of scope here (later slices): the repo app type, the explorer, meta render reuse, sphere/satellite reach. Publishing through outpost is unchanged: `outpost apps add` still points at a loopback URL. Verifying that a console sub-path publishes cleanly is S499.4's re-run.

Design of record: the operator's private sprint-499 story S499.6 (cb1286941710), sections "Design" and "Slices".
