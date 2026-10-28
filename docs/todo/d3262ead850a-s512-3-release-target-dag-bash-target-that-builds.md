---
id: d3262ead850a
kind: task
title: 'S512.3 release target: dag/Bash# target that builds, versions, packs and (later) publishes the live Chrome extension (zip + CRX, Web Store upload as a gated step)'
seq: 2821
status: done
priority: p1
labels:
    - browser
    - release
created: 2026-10-28T18:19:59.916433Z
assignee: s512-claude-manager
sprint: 512
sprint_id: 9f31a532-c25d-5504-8bd8-163f39bd871d
sprint_title: 'bashy browser site skills: the Bash# skill/script duality applied to web pages (thin extension core + per-site skills, self-healing)'
closed: 2026-10-28T18:29:11.285643Z
closed_by: s512-claude-manager
---

Operator 2026-10-28. A dag.md / Bash# target in yoke that: reads the version from manifest.json and checks it against the hub floor; runs the extension tests; packs a reproducible zip (sorted entries, fixed mtimes) plus sha256 into a release dir; optional CRX pack when a signing key is supplied through bashy secret (never in the repo); Chrome Web Store upload is a separate human-gated (@confirm) step for v1.0.x — the listing removes the remaining Developer-mode/Load-unpacked step. Effects declared per target. Test: the target runs locally and the zip is byte-identical across two runs.
