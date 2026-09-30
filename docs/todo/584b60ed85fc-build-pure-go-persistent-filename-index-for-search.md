---
id: 584b60ed85fc
kind: feature
title: Build pure-Go persistent filename index for search --files
seq: 5
status: done
priority: p1
labels:
    - search
created: 2026-09-30T05:14:32.190855Z
assignee: codex-gpt6-sol
sprint: 334
sprint_id: 2c3a1d0f-f046-5e3b-b086-b6b7a20505bd
sprint_title: Indexed filename search through bashy search --files
closed: 2026-09-30T05:29:21.080534Z
closed_by: codex-gpt6-sol
---

Implement persistent SQLite filename index in yoke/pkg/search using modernc.org/sqlite, with root-scoped storage, transactional rebuild/refresh, filename query without a crawl, bounded deterministic results, status including bytes, and tests. Preserve existing file scan fallback for unindexed roots. EverythingX inspired the approach (MIT, https://github.com/AlanKK/everythingx); record observed benchmark and index size.
