---
id: f8832a5cfe98
kind: feature
title: 'execlog: repeated failures become kb pitfall candidates automatically (learning without a human in the loop)'
seq: 3091
status: todo
priority: p3
labels:
    - execlog
    - kb
created: 2026-11-10T20:06:41.460928Z
sprint: 611
sprint_id: 7de44c27-d277-58b6-b42d-e5fabc9d668f
sprint_title: 'OTel logs signal: wire outpost''s slog bridge and drain the bashy span spool into the OTel log store'
---

Learning today is manual: kb observe / validate --from-gate / note add --candidate are agent decisions, and the execlog-to-kb pipe (pkg/execlog/tokb.go) runs only when invoked. The one automatic learner, execlog.Transitions, feeds next-command hints, not pitfalls. Scope: a bounded, idempotent pass (run by kb retro and by graph history, never per command) that finds templates failing with the same exit code in N distinct episodes within the window (defaults N=3, 7d), excluding the benign-probe set once Sprint 590 lands it (until then: grep, rg, diff, cmp, test, pgrep by name), and files ONE candidate pitfall per template in the host ring through kb.NearDuplicate (update the existing page's count and last-seen, never a duplicate), carrying the claim plus the address (template, episodes, first/last seen), never the records. Surface them in graph pitfalls and kb context --for. Verify: three sandbox episodes failing the same command produce exactly one candidate page; a fourth updates it; a validated page stops being re-filed.
