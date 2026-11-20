---
id: a4ef4a4272fc
kind: bug
title: '2 yoke tests fail on clean main: TestRedactCrossHostBodyValuesNotShapes (pkg/bus), TestInvokeRefusesClaimedLocalModelBeforeRunner (pkg/chat)'
seq: 3133
status: done
priority: p2
labels:
    - tests
created: 2026-11-20T03:41:06.960986Z
assignee: claude-opus5.5-s676
sprint: 676
sprint_id: cc17f40d-d5ff-5d27-bbed-b8563759cde6
sprint_title: 'Fleet hardening after the tool-shortcut tests: steerable ycode/zcode/dsh + pre-existing test failures'
closed: 2026-11-20T16:01:39.334297Z
closed_by: claude-opus5.5-s676
---

Seen 2026-11-19 while verifying weave run 69; both fail on clean main (GOWORK=off go test -run <name>). pkg/bus: RedactCrossHostBody = 'token [redacted:DEPLOY_TOKEN] and a bare [redacted:openai-key] word' (expectation mismatch). Root cause per test (product / test / venue) before any fix.
