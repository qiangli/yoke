---
id: bda5793006af
kind: bug
title: 'shaped apps: a unit with no declared effects runs at read+write; shapeware needs default-deny'
seq: 3157
status: todo
priority: p1
labels:
    - shaping
    - security
created: 2026-11-25T16:05:10.152714Z
sprint: 721
sprint_id: 69fc2c51-8faf-58e7-83ac-f27007837ae7
sprint_title: 'Shapeware pilot on form: spec v0 layout, one-step launch'
---

From the umbrella Sprint 712 vendor review (codex #1, muse #3; operator-accepted m2; spec v0.1 §7, finding F8).
`pkg/webconsole/shaped.go` runs a unit that declares no effects at {"read","write"} (overridden only when EffectCap() is non-empty). For shapeware, silence must not grant write: the spec says a missing cap means read-only and no field building.
Root cause first: confirm which apps rely on the read+write default today.
Proposed change: for `type: shapeware` apps, a unit with no declared effects runs read-only; other apps unchanged until reviewed.
Acceptance: unit test (no effects declared: write refused for a shapeware app; unchanged for others); existing webconsole tests pass.
