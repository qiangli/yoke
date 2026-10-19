---
id: 10e532fac3b7
kind: bug
title: Restore no-cgo embedded Ollama server model load via signed runner
seq: 2020
status: wontfix
priority: p1
labels:
    - sphere
created: 2026-10-13T16:47:51.425699Z
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T18:42:19.656432Z
---

Source qualification Yoke docs/validation/ollama-runner-source-2018.md identified llm/llama_nocgo.go returns an error and llm/server.go returns before external runner startup; JSON grammar nil path also needs accurate behavior. Implement a first-party Yoke server adapter or bounded supported-model path without modifying official v0.21.2 runner source, preserving Rule 1. Focused Go tests prove current binary reaches verified external runner for a supported tiny fixture, and fail clearly for unsupported tokenizer/schema. No dev-host native build/full suite; worker commits with trailers, no push.

Merged into Sprint 436 parent story 732cbb22d353 (signed embedded runner shipped). Preserve existing commits, fork tags, failed-run evidence and this checklist under the parent; this child is not independently accepted. Required release targets are Linux amd64/arm64, Darwin arm64, and Windows amd64; Darwin amd64 is optional. The operator restored Windows to Sprint 436 scope.
