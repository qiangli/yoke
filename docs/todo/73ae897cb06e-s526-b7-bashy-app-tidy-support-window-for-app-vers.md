---
id: 73ae897cb06e
kind: feature
title: 'S526.B7 bashy app tidy: support window for app versions; app-defined cleanup'
seq: 2919
status: todo
priority: p2
labels:
    - shaping
created: 2026-10-29T19:22:32.108382Z
sprint: 526
sprint_id: a9ea1d04-f39a-569f-8949-81dcb6aa864a
sprint_title: 'Software Shaping dogfooding: all eligible Bashy built-in apps'
---

Wave 2, B7 (after B1a, merged). Operator 2026-10-29. Add 'bashy app tidy <app> [--user U] [--apply]' (yoke pkg/webconsole app command tree; history in pkg/src history.go/prune.go), preview by default, like end-of-life support windows. Retention policy: keep last N, last T days, named versions, and any adopted/offered version (always kept). End-of-life history is squashed into one base version (history still restorable for every kept version). If the app declares a tidy unit (api/tidy.md or a declared unit name in shaped.json), call it through the app's agreed interface for app-specific cleanup of its own data. Offer the explicit data-contract step (B6 Contract) only when no supported version still needs the old property. Own versions only; never another user's data. Same on every OS. Acceptance: focused red/green Go test on a shaped fixture: preview lists what would be squashed and changes nothing; --apply squashes; named + adopted kept; tidy unit invoked; every supported version still restorable; another principal's history untouched; go build ./... green. Live proof on the console app is the manager's (B3). Evidence: dhnt docs/sprint-526-evidence.md (manager).
