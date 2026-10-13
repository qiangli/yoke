---
id: a2945711ef87
kind: spike
title: Qualify policy-compliant Ollama server path for signed embedded runner
seq: 2021
status: done
priority: p1
labels:
    - sphere
created: 2026-10-13T17:02:28.129891Z
weave: 238
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T17:08:21.579041Z
closed_by: s436-codex-manager
---

Review isolated worker #237 Yoke 516935de/fork 8c117eac0 but do not merge/push: it adds non-security edits inside qiangli/ollama third-party server fork and full no-cgo package build still fails. Rule 1 in umbrella docs/bashy-service-supply-chain-policy.md requires official stable tag, no non-security divergence for third-party servers. Find a concrete way to run the official v0.21.2 signed runner and restore managed local inference with first-party Yoke/Bashy adapter code outside the Ollama source fork, or official unpatched server binary. Compare source, module/API and build feasibility; document precise candidate contract or unavoidable policy blocker. No Dragon native build/container/full suite, no source fork edits. Commit findings with trailers, no push.
