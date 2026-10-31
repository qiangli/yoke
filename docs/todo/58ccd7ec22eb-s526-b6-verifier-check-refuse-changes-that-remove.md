---
id: 58ccd7ec22eb
kind: feature
title: 'S526.B6 Verifier check: refuse changes that remove or rename stored data or interface elements'
seq: 2918
status: done
priority: p1
labels:
    - shaping
created: 2026-10-29T19:18:21.825605Z
weave: 741
assignee: claude-sonnet5.5
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
closed: 2026-10-31T00:47:22.836521Z
resolution: fixed
closed_by: claude-sonnet5.5-m
---

Operator 2026-10-29. Master plan section 'Data evolution: every change stays rollback-safe'. Add one check to the trusted verifier (S525 A2): compare the app's declared data shape and agreed interface (unit contract: flags, arguments, unit names, routes; plus declared stored properties) before and after a candidate. Removing or renaming any of them fails verification like any other check. Adding is fine. An explicit user-approved 'contract' step (dropping an old property once no restorable version needs it) is the only way to remove, and it is separate from ordinary changes. Minimal: declared shapes only, no inference over arbitrary stored data. Same behavior on every OS. Acceptance: red/green unit test for rename, remove and add; a live bashy app UI candidate that renames a stored property is refused with a clear reason, and its add-with-fallback version passes.
