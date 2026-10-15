---
id: 01a14083-aafb-7b9c-907d-9b1261a537f4
seq: 4
form: page
type: gotcha
title: Real-engine probe tests need a test-only sibling package
description: 'WHEN testing yoke usage-probe seeds end to end (YAML -> fleet -> llmbudget.Quota through the REAL Bash# runner): llmbudget''s internal tests snippet.SetRunner(nil) in cleanup and pkg/dag imports llmbudget, so neither pkg/llmbudget nor pkg/fleet tests can import dag without a cycle or a nil runner. Put the rod tests in a test-only directory (only _test.go files, e.g. pkg/llmbudget/probeseeds) that blank-imports cmds/all (in-process jq/cat/ls/grep/tail) and pkg/dag (registers the real runner); go build ./... skips it, go test runs it. Isolate with BASHY_* cleared + temp HOME; note Model.Active() requires a resolvable api_key_ref env for kind api (use kind subscription or set <REF>_API_KEY), and BASHY_FLEET_SEEDS=off also drops compiled-in PLANS, so plan-probe tests must keep seeds on and pin HOME instead.'
status: candidate
source:
    tool: opencode-glm-5.3-c
    host: dragon
    episode: weave-issue-263
created: "2026-10-15T17:02:13Z"
---
