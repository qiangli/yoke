---
id: c0b0013ec389
kind: feature
title: 'S525.A9p yoke: wire shaping to production gates (verifier, activation, launcher link)'
seq: 2906
status: done
priority: p1
created: 2026-10-29T09:29:20.633268Z
weave: 688
assignee: codex-gpt5.6-sol
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T09:42:49.682633Z
resolution: fixed
closed_by: codex-gpt5.6-sol-l
---

Part of S525.A9 prep (umbrella eb1be776e47b). yoke main c939ec6f has A6 shaped app kind (pkg/webconsole/shaped*.go) with fake Verifier/Activator hooks in shaped_candidate.go; apps serve does not wire real ones, so approve refuses.
Scope:
- apps serve (pkg/webconsole/cmd.go + shaped_candidate.go): production Verifier = pkg/shape/verify (checks frozen from base, evidence sealed via pkg/shape/evidence); production Activator = pkg/src per-user activation (A7) gated by src.Gate(candidate, base, principal). Approve => verify -> evidence -> Gate -> activate; any failure leaves active unchanged.
- Make src.Gate the default activate gate for shaped apps (fail-closed when no evidence).
- --shape-agent: launch env carries BASHY_DELEGATE_ALLOW=<tool:model> (genie/delegate refuses when unset).
- Launcher tile for a shaped app links to /shape/<app>/ (pkg/webconsole/artifact/app.js) with one small DOM/HTML test.
- pkg/shape/contain darwin sandbox: allow read+exec of the resolved trusted runner binary path (installed bashy may live under the user's home), nothing broader.
- Do NOT edit pkg/skills, pkg/accounts, panel_accounts/api_accounts (other lanes).
Acceptance (one focused red/green test): go test ./pkg/webconsole -run TestShapedProductionGates — real verifier+activator over the notes fixture: genuine change -> approve -> alice active moves, bob unchanged; candidate that edits checks / exits early -> refused, active unchanged; no evidence -> Gate refuses. Plus go test ./pkg/shape/... ./pkg/src green. Do not run the whole webconsole suite (TestScopeHoldsOnTheAPISurfaceToo hangs on the live board). Behavior-only docs/commits, no hostnames. Trailers Sprint: #525 / Story: #<seq> / Story-ID.
