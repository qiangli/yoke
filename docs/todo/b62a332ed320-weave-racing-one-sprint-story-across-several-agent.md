---
id: b62a332ed320
kind: enhancement
title: 'weave: racing one sprint story across several agents is refused (story claim taken from body trailers)'
seq: 3096
status: todo
priority: p2
labels:
    - weave
    - sprint
created: 2026-11-11T03:23:12.749925Z
---

Found in Sprint 603 (2026-11-10). A run body carrying Story-ID trailers makes weave start claim that story for the worker (pkg/weave/weave_worker_story.go weaveClaimWorkerStories); a second run with the same trailers fails with 'story X is held by Y'. The conductor playbook says 'Race, don't merge, competing attempts', but racing needs either trailer-free bodies (manager adds trailers at merge, as S603 did) or the loom arena (podman booths). Proposal: a 'weave start --race' (or run label) that skips the worker story claim and leaves attribution to sprint accept --agent; or let one story hold several racer runs. Also: the race winner's delivery has to be submitted under the losing claimant (sprint submit --as holder) and accepted with --agent, which is awkward.
