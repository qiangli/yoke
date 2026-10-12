---
id: f53b79090172
kind: bug
title: 'meet: muse and agy turns are captured as raw JSON event streams, not reply text'
seq: 1992
status: done
priority: p2
labels:
    - meet
created: 2026-10-12T11:09:05.802688Z
assignee: s9999-stream-rework
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:19:52.254537Z
closed_by: s9999-maintenance-manager
---

Meeting 2026-10-12 bashy-feed retrospective: muse-spark1.3 turn recorded raw reconciliation JSONL; agy-gemini3.8-flash turn recorded its step_update event stream (text only in text_delta fragments). claude/codex/opencode turns were clean. Expected: meet extracts the final reply text per vendor CLI output format. See docs/meetings/meeting-note-2026-10-12T04-03-bashy-feed-epic-retrospective-and-launch-readine.md.

Closed companion, consolidated into retained story: c9d998c23a5e. Shared stream decoder delivered together; companion c9d998c23a5e. Original UUID and separate acceptance evidence retained in docs/sprint-9999-evidence.md in the umbrella. All original criteria independently verified before closure.
