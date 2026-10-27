---
id: b04aaef00855
kind: bug
title: 'S517.2 Windows: tour fleet/model-ensure-denied exits 99'
seq: 2781
status: done
priority: p0
labels:
    - known-failures
created: 2026-10-27T17:04:26.889674Z
assignee: codex-gpt5.6-sol
sprint: 517
sprint_id: 4589176f-2dd8-5458-bf93-1a15066eb26c
sprint_title: 'known failures to zero: Windows fleet fences, engine cases on peers, transpile-lowered skip; no xfail without a story and a date'
closed: 2026-10-27T18:36:02.547025Z
closed_by: s517-manager
---

xfail on Windows (todo 79d2994c). Reproduce on the Windows host, find the cause (likely the same MSYS program path), fix, remove the marker. No skips or xfail markers added; remove the case's xfail marker in bashsharp-tour cases.tsv in the same delivery once it passes. Native verification on the Windows host required (host-access file ~/.bashy/s514-windows-test-host, outpost ssh key auth). No hostnames in code/docs.
