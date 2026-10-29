---
id: 5000a0129790
kind: feature
title: 'S525.A10w yoke: shaped app history view (list/preview/diff/name/restore/undo) + A6 accessible controls'
seq: 2911
status: done
priority: p1
labels:
    - software-shaping
created: 2026-10-29T10:21:27.523674Z
weave: 693
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T10:33:56.216769Z
resolution: fixed
closed_by: muse-conductor-q
---

Sprint 525, goal a10 (parent 671de6e307ac). The A10 yoke half (pkg/src History/Restore/Name/Diff + `bashy src` verbs, story 3561440d) is merged; the shaped app's page has no history view yet. Build it in pkg/webconsole on the existing shaped-app page (shaped.go / shaped_candidate.go, routes beside handleShapePage/handleShapeDiff), calling the pkg/src exported API. Do not add a second history store.

Scope:
- GET history for the signed-in principal: versions with who, when, request text, verifier result, evidence link, name, active marker.
- Preview any version without moving the active version (reuse the existing preview path).
- Diff any two versions (reuse renderDiff: server-side, escaped line by line).
- Name a version; restore a version (forward commit through the same A2 evidence gate as approve); undo a restore (restore the previous active).
- Refusals are shown with their reason and leave active unchanged: schema-incompatible restore; restore whose evidence fails current gates.
- Same principal scoping and revoked-grant rules as the existing shape routes (anon 403, other user 404). Mutating routes need the same CSRF/session protections the approve route has.
- A6 remainder while you are in this file: the shape page and the new history view need labelled controls (label/aria-label on every button and input, keyboard reachable, live region for status). A rejected preview leaves no workspace (assert it).

Acceptance: one focused red/green httptest over the real handlers, two principals on a temp store: alice activates 4 versions, lists them, restores 3 back, undoes the restore, names one, diffs two; bob's history and active version are unchanged and he cannot see alice's; incompatible restore and gate-refused restore are refused with active unchanged; user data dir untouched; reject cleans up; the controls carry accessible names. Run `go test ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair'` and `go test ./pkg/src ./pkg/shape/...`. Unit tests only on this host: no full yoke suite, no podman. Sprint/Story/Story-ID trailers; no hostnames in code or docs. Do NOT edit pkg/shape/verify, pkg/shape/evidence or the protected launcher auth/pairing files. S524.8 (accounts tile) also touches pkg/webconsole: keep your change to the shaped files plus route registration.
