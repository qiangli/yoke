---
id: 4c7cada31413
kind: bug
title: 'pkg/skills TestAgentHubSinks* fail: pure-Go jq lacks --arg'
seq: 2893
status: todo
priority: p3
created: 2026-10-29T07:39:24.062305Z
---

Pre-existing on yoke main 94c78b0f..c0a83cfd (seen 2026-10-29 during S525 A1 review): agenthub_sinks_test.go:124 sink script runs 'jq --arg', the pure-Go coreutils jq rejects --arg ('not every GNU flag is implemented'). Fix: implement jq --arg/--argjson in coreutils jq, or make the test sink avoid --arg. Verify: GOWORK=off go test ./pkg/skills -run TestAgentHubSinks.
