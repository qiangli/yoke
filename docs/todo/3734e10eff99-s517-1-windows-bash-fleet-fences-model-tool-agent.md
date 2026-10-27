---
id: 3734e10eff99
kind: bug
title: 'S517.1 Windows: Bash# fleet fences (model/tool/agent inline + embed) exec bashy via an MSYS /c/ path -> exec: executable file not found'
seq: 2780
status: done
priority: p0
labels:
    - known-failures
created: 2026-10-27T17:04:26.742149Z
weave: 630
assignee: claude-sonnet5.5
sprint: 517
sprint_id: 4589176f-2dd8-5458-bf93-1a15066eb26c
sprint_title: 'known failures to zero: Windows fleet fences, engine cases on peers, transpile-lowered skip; no xfail without a story and a date'
closed: 2026-10-27T18:07:56.76651Z
resolution: fixed
closed_by: claude-sonnet5.5-f
---

Tour 11-fleet model-inline, model-embed, tool-inline, tool-embed, agent-inline, agent-embed are xfail on Windows (todo 79d2994c). The fence runner passes '/c/Users/.../bin/bashy' (MSYS form) as the program path to Go exec. Resolve the program path to its native form before exec, reusing the S514.R4 helper (yoke pkg/spacetime LookPath / pathconv.NativePath); program path only, never argv (binmgr-only argv rule). Gate: the six cases pass on the Windows host with a bashy built from the umbrella workspace. No skips or xfail markers added; remove the case's xfail marker in bashsharp-tour cases.tsv in the same delivery once it passes. Native verification on the Windows host required (host-access file ~/.bashy/s514-windows-test-host, outpost ssh key auth). No hostnames in code/docs.
