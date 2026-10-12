---
id: 2dedd9def444
kind: test
title: broker env JSON fixture uses invalid legacy session handle
seq: 1993
status: done
priority: p1
created: 2026-10-12T15:15:21.755221Z
assignee: s9999-fixture-rework
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:26:16.16091Z
closed_by: s9999-maintenance-manager
---

Final maintenance gate found TestEnvJSONEnvelope fails on baseline because fixture uses sess-1 while session validator requires s-. Correct fixture and expected envelope to valid handle; preserve explicit invalid-session rejection coverage. Acceptance: TestEnvJSONEnvelope and session validation tests pass, no production validator relaxation. Evidence: baseline8235011 failure env_json_test.go:56 session handle sess-1 must start with s-. Coordinate in active broker revision run194, original sticky story remains independent.
