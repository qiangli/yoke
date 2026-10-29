---
id: 559f8f88434d
kind: bug
title: 'S525.A9b yoke: zero-setup shaped app registration (app add imports + base commit + pristine) and /shape.js under an app-scoped device grant'
seq: 2920
status: todo
priority: p0
labels:
    - software-shaping
created: 2026-10-29T17:01:05.646658Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Sprint 525, goal a9 (parent eb1be776). Bugs found by the live A9 smoke on the installed candidate (bashy 84be5506 / yoke c9e58916, Linux, elephant WSL). Operator rule: users never handle technical details, and any manual step is a bug.

(1) Registering a shaped app needs manual git surgery. The console requires the source to be the managed checkout $BASHY_HOME/src/<name> with a base commit and a `pristine` branch (src/activate.go: "no version line and no pristine branch"; serve logs "shaped app has no base revision"; api units return 503). Today the user must git init, commit and create the pristine branch by hand, and bashy's own git cannot even do the last step: `bashy git branch pristine` returns "not served by the native git engine".
Fix: `bashy app add <name> --type shaped --source <dir>` imports the app (dir or git URL) into $BASHY_HOME/src/<name>, makes the base commit with a bashy-owned author identity when none is configured, and creates `pristine`, all through go-git, idempotently. Registration then points at the managed checkout. Also make `bashy git branch <name> [<start>]` (create) work in the native engine, a small parity gap.

(2) Under a device pairing scoped to the app (`bashy app pair --allow notes`), GET /shape.js returns 403. The shape panel page loads, but its script is refused, so a scoped phone cannot use shaping. Add the shape panel's assets to the console-wide asset list in pkg/webconsole/scope.go, the same way /accounts.js was added. The shape API stays scoped.

Acceptance:
(a) A focused test: app add --type shaped --source <plain dir with the notes fixture> on a fresh BASHY_HOME with no git identity configured, then a notes-scoped device reads /notes/api/list and posts /notes/api/add (200), with no other setup.
(b) A notes-scoped device GET /shape.js gives 200, while GET /api/shape/<other app> stays refused.
(c) bashy git branch create test.
go test ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair' ./pkg/fleet ./pkg/src ./git/... green. Do not edit pkg/shape/evidence, pkg/shape/verify semantics or the protected launcher auth/pairing files (scope.go asset-list lines are fine). Sprint/Story/Story-ID trailers; no hostnames.
