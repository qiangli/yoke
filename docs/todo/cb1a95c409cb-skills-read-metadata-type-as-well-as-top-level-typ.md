---
id: cb1a95c409cb
kind: enhancement
title: 'skills: read metadata.type as well as top-level type (shapeware, strict skill validators)'
seq: 3155
status: todo
priority: p1
labels:
    - skills
    - shaping
created: 2026-11-25T15:38:59.990758Z
sprint: 721
sprint_id: 69fc2c51-8faf-58e7-83ac-f27007837ae7
sprint_title: 'Shapeware pilot on form: spec v0 layout, one-step launch'
---

Operator-approved follow-up from umbrella Sprint 712 (story b35b89620f88, review D19 and vendor review D20 item m6; finding F4 in umbrella docs/shapeware-spec-v0.md). Gates the Sprint 721 form pilot.

Today `pkg/skills/skill.go` (via `pkg/unit/frontmatter`) reads only a TOP-LEVEL `type` key (the OKF page type; dag files carry `type: dag`). The strict Agent Skills validator (skills-ref) rejects any top-level key outside name/description/license/compatibility/metadata/allowed-tools, so shapeware writes ONLY `metadata.type: shapeware`.

Change: read `metadata.type`; fall back to the legacy top-level `type` when `metadata.type` is empty (compatibility, e.g. dag files). When both are set and differ, use `metadata.type` and surface a warning (the shapeware lint fails on it). No other behaviour changes; dag files keep working.

Acceptance: unit tests in pkg/skills: (1) top-level only: that value; (2) metadata only: the metadata value; (3) both equal: that value, no warning; (4) both differ: the metadata value plus a warning; (5) neither: empty. Run the existing pkg/skills and pkg/unit/frontmatter tests.
