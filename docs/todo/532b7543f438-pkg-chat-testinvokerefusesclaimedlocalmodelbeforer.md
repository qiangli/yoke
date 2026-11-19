---
id: 532b7543f438
kind: bug
title: pkg/chat TestInvokeRefusesClaimedLocalModelBeforeRunner fails at HEAD on macOS
seq: 3129
status: todo
priority: p2
labels:
    - test
created: 2026-11-19T18:09:09.463354Z
---

Found during Sprint 671 targeted tests (2026-11-19). Fails at unchanged yoke main f4a89e59 on the macOS dev box, also under env -i (HOME, PATH, GOWORK=off): the holder branch at claim_launch_test.go:110 is refused its own model: 'llmbudget: other already holds model:gpt-oss:20b on h (eval) ... live'. The first half (non-holder refused) passes. Not caused by Sprint 671. Root cause NOT yet identified: follow the root-cause-before-fix order (compare with last green run, check venue, bisect, e.g. around 2450ce05 claims: refuse a launch of a claimed model on every billing lane) before any change.
