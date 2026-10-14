---
id: 3054b43bab36
kind: task
title: 'Ollama: remove the downloaded upstream-archive fallback; embedded source-built runner only (fail closed)'
seq: 2005
status: done
priority: p2
labels:
    - supply-chain
created: 2026-10-12T21:33:52.870293Z
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T04:05:45.328207Z
closed_by: s434-manager
---

Goal (S434 migrate-servers-services, audit row docs/bashy-supply-chain-audit.md:22 in the umbrella): bashy must never exec a third-party-built ollama. Keep ONLY the embedded runner compiled from source (external/ollama: runner_embed, src, ollama_component.go). Remove/disable the downloaded official-archive fallback.

Where: external/registry/ollama.go (GitHubSpec ollama/ollama v0.40.1 download), pkg/binmgr/pins.go lines ~123-129 (ollama@v0.40.1 sha256 pins), and any caller that falls back to the downloaded binary (grep registry.Ollama / OllamaVersion / "ollama/ollama" outside external/ollama/src).

Acceptance:
1. No code path downloads or execs an upstream ollama release archive; `bashy ollama` / managed service use the embedded server only. If the embedded build is unavailable on a platform, fail closed with a clear error naming the supply-chain policy (docs/bashy-service-supply-chain-policy.md) - never PATH or download fallback.
2. Remove the ollama@v0.40.1 pins (or leave them only if still referenced by a test that proves refusal).
3. Keep upstream update/version checks off (OLLAMA_NO_CLOUD etc. already in external/ollama/managed.go - do not regress).
4. Unit test proving the refusal (no network). Gate: go vet + go test -count=1 ./external/ollama/... ./external/registry/... ./pkg/binmgr/... green (focused only; no full suites, no containers).
5. Commit with Sprint/Story trailers; then `bashy sprint submit` with the commit SHA and the gate output.
