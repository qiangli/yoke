---
id: 53d8ad7c0e7a
kind: bug
title: 'S2 Windows chat native gate: attach socket timeout and path/redaction failures'
seq: 1895
status: done
priority: p1
labels:
    - windows
    - chat
created: 2026-10-11T08:44:25.218206Z
weave: 94
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T09:00:24.575763Z
resolution: fixed
closed_by: codex-gpt6-sol
---

Scoped native Windows acceptance child of S2 64aa86371cc1 and S10 ACP transport. Claimed noviwin1.local exact yoke 8a1ff48 GOWORK=off go test -timeout 20m ./pkg/chat/... failed at 08:44 UTC: TestAttachCoachSteersThroughSocket hung 19m50s until 20m timeout; TestCheckACPAdapterAvailability fake adapter not detected; TestInvoke* output spill/redaction leaked raw Windows path; TestInvokeRefusesClaimedLocalModelBeforeRunner and ResolvedModelID attempted colon-containing filename invalid on Windows. Raw gate /tmp/s418-windows-yoke-8a1ff48.log. Fix package source or portable test fixture as appropriate, preserve actual permission/security behavior and deny-by-default. Run focused native Windows tests on claimed host, then pkg/chat full package; Mac/Linux focused regression. Do not edit pkg/fleet or pkg/toolcmd (other active lanes). No false native green on a subset. Commit with Sprint #418 and this Story-ID; manager reviews exact diff and native output.
