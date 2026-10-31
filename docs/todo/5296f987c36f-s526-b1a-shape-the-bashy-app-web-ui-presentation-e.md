---
id: 5296f987c36f
kind: refactor
title: S526.B1a Shape the bashy app web UI presentation (embedded SPA -> shaped kind)
seq: 2875
status: done
priority: p1
labels:
    - software-shaping
created: 2026-10-28T23:28:40.819079Z
weave: 740
assignee: claude-fable5.1
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
closed: 2026-10-31T01:16:16.404763Z
closed_by: s526-manager
---

Wave 1, B1a (hard). Plan: dhnt docs/sprint-526-software-shaping-plan.md, B0 record D2/D3. The bashy app web UI is a Go-embedded SPA (yoke/pkg/webconsole/artifact/* via embed.go). Convert its PRESENTATION to the S525 shaped kind (pkg/webconsole/shaped.go): the embedded artifact bytes become the base commit of the app's src checkout; the launcher serves each principal's active tree and falls back to the embedded bytes when the principal has none. Shaped pages: index, desktop, board, inbox, mb (html/js/css). Go API handlers stay Go. Protected (D3): add ProtectedManifest rows (yoke/pkg/src/manifest.go) for pkg/webconsole/artifact/{shape,term,accounts}.* so a principal tree can neither override nor drop them; launcher auth/pairing/gate files are already protected. No privileged bypass: use the same public shaped mechanisms external apps use; a missing capability becomes a generic extension. Default containment only (no @sandbox() units until S539.F8). Acceptance: one focused red/green Go test: (a) principal without a tree gets the embedded bytes, (b) a principal revision of desktop.html is served only to that principal, (c) a principal tree touching shape/term/accounts is refused. go build ./... green in yoke. Evidence: dhnt docs/sprint-526-evidence.md (manager writes it; report test names + commit). Early live check on zoo follows (manager).
