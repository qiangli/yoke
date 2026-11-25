---
id: cb1a95c409cb
kind: enhancement
title: 'skills: read metadata.type as well as top-level type (shapeware, strict skill validators)'
seq: 3155
status: todo
priority: p3
labels:
    - skills
    - shaping
created: 2026-11-25T15:38:59.990758Z
---

Operator-approved follow-up from umbrella Sprint 712 (story b35b89620f88, review D19, finding F4 in umbrella docs/shapeware-spec-v0.md §16).

Today `pkg/skills/skill.go` (via `pkg/unit/frontmatter`) reads only a TOP-LEVEL `type` key (the OKF page type; dag files carry `type: dag`). The strict Agent Skills validator (skills-ref) rejects any top-level key outside name/description/license/compatibility/metadata/allowed-tools, so a shapeware release writes `metadata.type: shapeware` (the spec's one favoured spelling).

Change: when the top-level `type` is empty, take `metadata.type`. The top level wins when both are set (the same rule bashy's dag docs already state for dag-only keys, bashy/docs/dag.md). No other behaviour changes.

Acceptance: a unit test in pkg/skills: (1) top-level only gives that value; (2) metadata only gives the metadata value; (3) both set: top level wins; (4) neither: empty. Run the existing pkg/skills and pkg/unit/frontmatter tests.
