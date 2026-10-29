---
id: fd74138bc66b
kind: feature
title: 'S525.A2b yoke: skills Bash# repair overlay through the trusted verifier; unit contract reader'
seq: 2907
status: done
priority: p1
created: 2026-10-29T09:29:28.104916Z
weave: 689
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T09:45:04.931161Z
resolution: fixed
closed_by: muse-conductor-m
---

Part of S525.A2 follow-up (umbrella 6bd2374295a2) + A1 (b976805c404f). yoke main c939ec6f.
Scope:
- pkg/skills/bashsharp.go learned-overlay repair path: a repaired candidate body is promoted to the overlay ONLY after pkg/shape/verify passes it (checks/contract frozen from the original unit body, call accounting via verify.CallLogEnv), with an evidence record bound to the overlay body identity; a stale/missing/failed record => run the original body, never the overlay.
- pkg/shape/verify ContractReader: add a reader backed by the A1 pkg/unit contract reader (@require @ensure @guard @effects @confirm @timeout, both forms); keep the minimal reader as fallback only where pkg/unit has no contract.
- Do NOT edit pkg/webconsole, pkg/src activation, pkg/accounts (other runs).
Acceptance (one focused red/green test): go test ./pkg/skills -run TestBashSharpOverlayVerified — a weakened @ensure repair, an early-exit repair and an uncalled-contract repair are NOT promoted; a genuine repair is promoted and runs next time; plus go test ./pkg/skills ./pkg/shape/... ./pkg/unit green (TestAgentHubSinks jq --arg is pre-existing red, todo 4c7cada3). Behavior-only docs/commits, no hostnames. Trailers Sprint: #525 / Story: #<seq> / Story-ID.
