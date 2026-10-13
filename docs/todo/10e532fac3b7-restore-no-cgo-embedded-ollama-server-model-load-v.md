---
id: 10e532fac3b7
kind: bug
title: Restore no-cgo embedded Ollama server model load via signed runner
seq: 2020
status: assigned
priority: p1
labels:
    - sphere
created: 2026-10-13T16:47:51.425699Z
weave: 237
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
---

Source qualification Yoke docs/validation/ollama-runner-source-2018.md identified llm/llama_nocgo.go returns an error and llm/server.go returns before external runner startup; JSON grammar nil path also needs accurate behavior. Implement a first-party Yoke server adapter or bounded supported-model path without modifying official v0.21.2 runner source, preserving Rule 1. Focused Go tests prove current binary reaches verified external runner for a supported tiny fixture, and fail clearly for unsupported tokenizer/schema. No Dragon native build/full suite; worker commits with trailers, no push.
