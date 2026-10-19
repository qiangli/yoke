---
id: fe7ef21ca333
kind: spike
title: Qualify official Ollama stable source for embedded runner
seq: 2018
status: done
priority: p1
created: 2026-10-13T16:34:05.307885Z
weave: 235
assignee: codex-gpt6-astra
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T16:44:42.437255Z
closed_by: s436-codex-manager
---

For parent 732cbb22, find an official stable ollama/ollama release tag that has a buildable separate llama.cpp runner compatible with current Yoke embedded server protocol, across Linux amd64/arm64, Darwin amd64/arm64 and Windows amd64. Current fork gitlink 43768325 has cmd/runner/main.go, but official v0.40.3 eab97e9f has no cmd/runner, so draft Bashy runner branch cannot be published under Rule 1. Inspect tag ancestry/delta, source license/dependencies and platform support without dev-host native build/container/full suite. Deliver precise source pin and technical implementation path or concrete blocker; do not weaken official stable tag policy. Commit worker findings with story trailers, no push.

Worker findings: docs/validation/ollama-runner-source-2018.md and companion JSON.
Source-only gate PASS for official v0.21.2 exact SHA and all five dependency
graphs; native/runtime/audit/signing gates remain with parent 732cbb22.
