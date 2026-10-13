---
id: da3ef757fb91
kind: bug
title: Fail closed managed Ollama serve until a bashy-signed embedded runner is available
seq: 2012
status: done
priority: p0
labels:
    - ollama
    - policy
created: 2026-10-13T08:41:18.022391Z
assignee: codex-gpt5.6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T08:52:29.350977Z
closed_by: s436-codex-manager
---

Rule1 hard gate: current yoke external/ollama/cmd.go managed serve falls back to OLLAMA_BIN/PATH/cache when runner_embed.Available() is false; defaults now enable ollama but no signed runner has shipped. Scope: managed serve must refuse unverified system binary with clear error, including UseSystemBinaries path; leave deferred third-party fallback removal branch 6f38e3d4 intact and keep non-service CLI behavior as-is. Add regression that puts a fake executable on PATH/OLLAMA_BIN and proves serve does not invoke it without embedded runner. Tests: go test ./external/ollama focused and no Dragon heavy build. Commit with Sprint 436 trailers; no yoke main push by worker, manager integrates after independent review.
