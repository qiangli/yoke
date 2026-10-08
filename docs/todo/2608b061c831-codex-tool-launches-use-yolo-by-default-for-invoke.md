---
id: 2608b061c831
kind: bug
title: Codex tool launches use YOLO by default for invoke and chat
seq: 29
status: todo
priority: p1
labels:
    - codex
created: 2026-10-01T02:20:14.191817Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Operator instruction 2026-09-30: Bashy must launch Codex with the CLI approval-and-sandbox bypass for ordinary invoke and chat starts. Centralize at agentlaunch so named bindings, headless invoke, interactive chat, and steer receive the same flag. Preserve explicit read-only review and explicit safe sandbox overrides. Acceptance: focused argv tests show the bypass exactly once for ordinary Codex launches, no bypass for read-only or explicit safe sandbox, and no change to other tools; relevant Go tests pass.

Sprint 321 owns agent identity: do NOT edit yoke pkg/agentlaunch identity code or pkg/fleet types (family/instance); tool recipe YAML and launch flags are fine. If your fix needs those files, message codex-gpt6-sol (bashy mb send) first.

Worker rules (Sprint 379 conductor, 2026-10-08): work in the submodule named above; commit and push INSIDE it to its default branch (public repos: main), then stop - the conductor bumps the umbrella pin. Every commit carries Sprint: #379 plus Story/Story-ID trailers for this story, with BASHY_AGENT set to your own binding. Reproduce first with a red unit test, fix at the root, never skip, quarantine or add to a known-failures list. This dev box (Dragon) is for builds and focused unit tests only. Windows runs go to noviwin1.local, Linux runs to the novidesign.local podman machine bashy or the repo CI, and macOS full suites to novidesign.local (all passwordless ssh; use the .local names). Gate before push: go build ./... and go vet on the module, plus the focused tests, with the exit code captured (never gate a push on a pipe). Report: the root cause, the commit SHA, the exact tests run and where, and any CI run URL.
