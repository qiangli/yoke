---
id: d1db319676d1
kind: bug
title: 'S517.1b Windows: fleet cases exit 99 - transport/meter evidence lost via MSYS env paths in nested exec'
seq: 2782
status: done
priority: p0
labels:
    - known-failures
created: 2026-10-27T18:15:51.941446Z
weave: 632
assignee: claude-sonnet5.5
sprint: 517
sprint_id: 4589176f-2dd8-5458-bf93-1a15066eb26c
sprint_title: 'known failures to zero: Windows fleet fences, engine cases on peers, transpile-lowered skip; no xfail without a story and a date'
closed: 2026-10-27T18:23:02.312241Z
resolution: fixed
closed_by: claude-sonnet5.5-h
---

Residue of S517.1 (3734e10e, merge ab16f8bc on main b7402c75). Manager elephant evidence 2026-10-27: fence EXEcS now (model-inline prints recorded answer) but all 7 fleet cases exit 99: fixture evidence check - transport.calls never written (reply.bsh appends >> $FLEET_TOUR_CALL_LOG, an MSYS /tmp path, inside nested bashy exec), meter.json lacks day_tokens (BASHY_LLM_BUDGET_STATE same MSYS path). Trace env propagation + MSYS file-path handling in nested exec (yoke chat/broker/llmbudget + shell redirect), fix at right layer reusing pathconv, add unit test (pkg/spacetime lookpath_test pattern), justify/revert llmbudget exec change in ab16f8bc. Native verify on ELEPHANT (windows-big ~/.bashy/release-hosts; staged ~/s517-qa): TOUR_FILTER=fleet/ check.sh 7 xfail cases must XPASS; do NOT edit cases.tsv (manager removes markers). Coordinate with S517.2 #631 (same bug, model-ensure-denied) - one fix, no duplication. No hostnames in code/docs.
