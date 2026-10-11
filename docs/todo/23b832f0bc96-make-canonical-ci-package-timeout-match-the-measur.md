---
id: 23b832f0bc96
kind: test
title: Make canonical CI package timeout match the measured serial weave suite
seq: 40
status: assigned
priority: p2
labels:
    - ci
created: 2026-10-11T00:17:44.788625Z
weave: 66
assignee: codex-gpt6-sol
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

Necessary Sprint401 gate follow-through: full unchanged CI scope passes5387 top-level tests/0failures under an explicit external45m package bound. Actual serial pkg/weave takes about18min, exceeding Go default10min, and all defaultCI OS jobs timed out. Make the canonical scripts/ci-test-gate.sh primary go test invocation explicitly bounded45m so CI matches measured gate. Scope only that flag plus explanatory comment; preserve package selection, confirming reruns, known-failure baseline, all assertions/JSON extraction/exit logic. No productionGo/UI/timeout grace changes. Implementer must use isolated yoke weave, selfclaim and selfsubmit Story9999 canonical --repo /Users/qiangli/projects/poc/dhnt/yoke, with true injectedAgent and Sprint/Story/Story-ID trailers. Dragon source/shellsyntax only; no full Go/DOM/builds. Root s401-manager owns nativehosts and reruns canonical gate WITHOUT anyGo wrapper on Linux. Bound5min,1point; independent root review and actual native gate before accept. Public story: no private umbrella evidence, secrets or proprietary code. Commit source insideyoke, no pushes/canonical edits. Preserve injected trackednotes beforecommit. Root publishes after verification.
