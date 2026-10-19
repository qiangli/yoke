---
id: c9d998c23a5e
kind: bug
title: Muse and AGY stream decoding for coach and Meet
seq: 1961
status: done
priority: p2
created: 2026-10-12T07:21:43.860424Z
assignee: s9999-stream-rework
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:19:51.582816Z
closed_by: s9999-maintenance-manager
---

Sprint 423 yoke run 152 (muse-spark1.3): 938 lines of correct code + passing tests in 4 min, then [coach] UNRESOLVED LOOP - 3 interventions over 1113 output lines (1113 distinct, ~290k output tokens) and context cancelled (exit 143). muse emits JSON event lines; every line is distinct, so distinct-line count is not a loop signal. Work survived only as a wip commit without trailers, forcing a follow-up run.

S9999 consolidated stream lane includes #1992 f53b79090172 meet extraction of Muse reconciliation JSONL and AGY step_update/text_delta into final reply text. Retain real JSON answers and plain text; do not expose tool/thinking/usage. Coaching must measure semantic progress, never unique event IDs as loop evidence; actual repeated unproductive cycles remain detectable. Locate existing vendor decoders and reuse when appropriate. Scope meet/normalize, chat stream decoder and weave coach only; avoid weave_impl.go hydration/identity work. Read CLAUDE.md. Focused regression tests only on dev-host; no containers/full build/push. Commit both story trailers, correct own identity and send evidence to s9999-maintenance-manager.

Consolidated retained story: c9d998c23a5e. Shared stream decoder delivered together; companion f53b79090172. Original UUID and separate acceptance evidence retained in docs/sprint-9999-evidence.md in the umbrella. All original criteria independently verified before closure.
