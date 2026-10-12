---
id: d364f12fcc99
kind: bug
title: weave run ends 'submitted' but never sprint-submits the story when a weave(auto) commit follows the worker commit
seq: 1967
status: todo
priority: p2
created: 2026-10-12T07:41:51.549788Z
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

S423 yoke run 155 (muse): worker commit a580d6f carried Story trailers, weave(auto) then committed KB.md; weave list shows submitted but no 'submitted story' event on sprint 423 and the story stays assigned, so sprint accept refuses. Run 154 (no auto commit) submitted fine.
