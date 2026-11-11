---
id: f2b2bf1fd8a2
kind: bug
title: broker/foreman tests fail under heavy parallel load (pass isolated)
seq: 3097
status: todo
priority: p2
created: 2026-11-11T12:33:13.109063Z
---

Sprint 613 rc gate, Windows test host WSL, yoke d0ba2a11, full non-external run concurrent with three bashy go shards: pkg/broker TestPeerPrincipalEvictsOwnerResidentModels (isolation_test.go:307 'peer turn audited as owner') and TestAuditFileCarriesCostFields (price_test.go:351 '1 priced, 0 unpriced; want 1 and 1'), pkg/foreman TestPromptReachesSteadyCeilingOverLongHistory (continuation_test.go:59 'llmbudget: meter busy: lock held'). All three pass with -count=1 isolated at d0ba2a11 and at rc.30 yoke 3544f295. Root cause not yet identified: the broker audit attribution one may be a real ordering race worth reading first. Verification: -count=20 -cpu 1,8 under load (stress) red before, green after.
