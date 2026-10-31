---
id: 315705acc9c4
kind: feature
title: 'S526.B1g Common Genie button: one protected component embedded on every shaped app page'
seq: 3044
status: todo
priority: p1
labels:
    - shaping
    - launcher
created: 2026-10-31T06:50:55.875784Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Operator approved 2026-10-31 for Sprint 526 (new feature added mid-sprint with explicit operator approval).

Today the Genie (sparkle) button exists only in the launcher header; the desk apps (notepad, form) have no in-app way to start a wish.

Build one standard component:
- One small plain JS + CSS asset (no framework, no CDN, no new dependency), served by the launcher at a fixed protected path (e.g. `/_shape/genie.js`). It draws the sparkle button and opens the wish panel as an overlay on the current page.
- The launcher injects it into every shaped app page it serves (launcher, notepad, form); apps need no change. The same seam later serves proxied external apps (S527).
- Context: the wish carries the app, the current page/route and, optionally, an element the user points at.
- Protected: part of the shaping plumbing (with the Shape page), not changeable through personal shaping; listed in the protected manifest.
- One look and behavior on every app and OS, following the launcher design system.
- Works behind the zoo prefix (`/matrix/h/<host>/app/<name>/`), including inside previews (see F1 in docs/sprint-526/operator-findings-2026-10-31-wish-page.md).

Placement (operator agreed 2026-10-31):
- On every page of every shaped app, as a small corner button; a wish still targets the app (the app is the shaping unit; the page and element are context).
- Per-app setting to hide it, plus a keyboard shortcut; when hidden it stays available from the app's landing page.
- Protected pages (Shape page, sign-in/pairing, term, accounts): the button explains the page is protected and files the wish as a request instead of building a candidate (fixes F2).
- Inside a preview: the button is disabled with "you're previewing a candidate".

Acceptance: red/green tests (injection on each shaped app page; the protected path cannot be changed by a candidate; the wish carries app + page). Live on zoo: the operator starts a wish from inside the form app (wish #1, "Other" with a reason box) and from the launcher, both through the same button.


Manager constraints (S526): yoke pkg/webconsole (inject the component where pages are served: servePageFile / shaped app page path / preview); add the protected path to pkg/src ProtectedManifest; reuse F3a preview handling (yoke e0898d17). NO podman/docker/make test/full suites on Dragon: focused go test + go vet only. Desk apps (notepad, form) are shaped apps served via the public path; inject there too. Manager runs the live zoo check.

Operator update 2026-10-31: button TOP-RIGHT by default; its position and hidden state come from the app's shaped config (e.g. a small shaped.json / app config key), so a wish can move or hide it while the component itself stays protected.

Placement decisions of record: dhnt docs/sprint-526/b1g-genie-button-placement-2026-10-31.md @ 2a3a694f1 (every page; top-right default; shaped.json genie:{position,hidden} read by the protected component; shortcut always opens; protected pages file a request; preview disables). Acceptance addition: a wish to move/hide the button yields a config-only candidate that passes Verify, component unchanged.
