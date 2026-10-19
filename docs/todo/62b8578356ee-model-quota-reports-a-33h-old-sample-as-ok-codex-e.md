---
id: 62b8578356ee
kind: bug
title: model quota reports a 33h-old sample as ok; codex exhaustion went undetected
seq: 2031
status: done
labels:
    - quota
created: 2026-10-19T03:12:31.171502Z
assignee: codex-gpt6-sol
sprint: 469
sprint_id: ce3c3f37-86a2-5069-a2b7-043f2f9be051
sprint_title: Bashy model quota accuracy fixes
closed: 2026-10-19T05:33:50.899751Z
closed_by: codex-gpt6-sol
---

Observed 2026-10-18 20:12 PDT on dev-host: bashy model quota showed codex chatgpt-pro week 17% STATUS ok with AGE 33h22m (source probe:plan:chatgpt-pro/sessions) while the operator's Codex quota was actually exhausted (operator reset it). Earlier in Sprint 446 codex workers died quota-exhausted (run 6, run 497 takeover). Defects: (1) a sample far older than its window is still rendered ok instead of stale/unknown; (2) no 5h window row for codex; (3) a quota-exhausted worker death does not update the codex row; (4) --refresh fails on model-free hosts (todo 76128d5f), so stale rows never refresh. Expected: STATUS degrades to stale past a freshness bound, vendor-error exhaustion events update the row, and the window set matches the plan. Fix with tests.
