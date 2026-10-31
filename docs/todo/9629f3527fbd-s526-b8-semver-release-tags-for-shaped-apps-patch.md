---
id: 9629f3527fbd
kind: feature
title: S526.B8 Semver release tags for shaped apps (patch=repair, minor=additive, major=contract)
seq: 2920
status: todo
priority: p2
labels:
    - shaping
created: 2026-10-29T19:28:23.720198Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Wave 2, B8 (after B6, merged). Operator 2026-10-29; master plan 'Releases and dependencies'. Releases of a shaped app are semver tags in its src history (yoke pkg/src: history.go, offer.go, cmd_history.go/cmd_offer.go). Bump from the change kind: patch = repair, minor = additive enhancement, major = explicit contract step / removal. Derive 'additive' vs 'removal' from the B6 surface compare (pkg/shape/verify surface.go: CompareSurface / Contract): no removed surface element = minor (or patch when the candidate is a repair), a caller-approved Contract drop = major. Untagged versions show Go-style pseudo-versions (v0.0.0-<UTC yyyymmddhhmmss>-<12-hex>); offerings (offer.go) publish tagged releases only. Same on every OS. Acceptance: one focused red/green Go test on a shaped fixture: first release v1.0.0, a repair -> v1.0.1, an additive change -> v1.1.0, a rename refused unless released as v2.0.0 through the contract step; untagged version prints a pseudo-version; go build ./... green. Live proof on the console app is the manager's (B3). Evidence: dhnt docs/sprint-526-evidence.md (manager).
