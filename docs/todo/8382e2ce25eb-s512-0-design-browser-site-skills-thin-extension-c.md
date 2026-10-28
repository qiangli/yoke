---
id: 8382e2ce25eb
kind: task
title: 'S512.0 design: browser site skills (thin extension core + per-site Bash# skills, self-healing via local PRs)'
seq: 2772
status: done
priority: p1
labels:
    - browser
created: 2026-10-27T01:06:39.873318Z
assignee: s512-claude-manager
sprint: 512
sprint_id: 9f31a532-c25d-5504-8bd8-163f39bd871d
sprint_title: 'bashy browser site skills: the Bash# skill/script duality applied to web pages (thin extension core + per-site skills, self-healing)'
closed: 2026-10-28T18:29:09.786628Z
closed_by: s512-claude-manager
---

Operator idea 2026-10-26 after Sprint 511 bugs: instead of hardcoding page logic in the MV3 extension, run per-site skill/script code chosen by the current page, and let the agent heal/extend it through the same channel. Decide: execution path (chrome.userScripts in USER_SCRIPT world, one-time user opt-in; or CDP probe mode), skill format and lookup (by origin/path), effect envelope per site (allowed origins/actions, receipts), human-only steps, test fixtures, how a heal becomes a test-gated local PR. Output: a design doc and the MVP story list.
