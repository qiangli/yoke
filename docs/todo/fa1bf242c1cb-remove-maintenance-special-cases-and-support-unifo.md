---
id: fa1bf242c1cb
kind: refactor
title: Remove maintenance special cases and support uniform sprint selection
seq: 2004
status: done
priority: p1
created: 2026-10-12T18:19:55.106846Z
weave: 215
assignee: s435-uniform
sprint: 435
sprint_id: a430889f-ae6c-5996-aa3e-94af7dcfe567
sprint_title: Uniform sprint behavior
closed: 2026-10-12T18:37:24.967529Z
resolution: fixed
closed_by: s435-uniform-g
---

Operator request: remove all Sprint 9999 maintenance special support and run sprints uniformly. Later steering: allow a sprint number, last, or next and default omitted selection to last. Implemented: no reserved number/fixed maintenance identity/lazy card creation; numeric or last selectors via sprint operands or --sprint flags; omitted selection picks most recently created existing card; empty-board error without auto-creation; repository todos index with existing selected cards, --no-sprint and personal standalone behavior retained; historical cards/story UUIDs preserved and hydrate ordinarily; active guidance updated. Core candidate Yoke 7fa0aeb1 / Bashy b82b92a6. Native todo/weave short tests passed on initial candidate; final selector regressions, format and Windows/Linux/Darwin/AIX checks passed. Consumer build and installed checks underway. Remaining clarification: does next create a new numbered sprint, or select the next existing sprint with unfinished work? Keep this one story open until that selector is resolved; no new follow-up stories.
