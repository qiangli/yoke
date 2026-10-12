---
id: c9d998c23a5e
kind: bug
title: weave coach kills a productive muse run as UNRESOLVED LOOP (stream-json lines counted as distinct output)
seq: 1961
status: todo
priority: p2
created: 2026-10-12T07:21:43.860424Z
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
---

Sprint 423 yoke run 152 (muse-spark1.3): 938 lines of correct code + passing tests in 4 min, then [coach] UNRESOLVED LOOP - 3 interventions over 1113 output lines (1113 distinct, ~290k output tokens) and context cancelled (exit 143). muse emits JSON event lines; every line is distinct, so distinct-line count is not a loop signal. Work survived only as a wip commit without trailers, forcing a follow-up run.
