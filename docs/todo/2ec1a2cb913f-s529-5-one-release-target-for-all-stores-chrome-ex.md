---
id: 2ec1a2cb913f
kind: task
title: 'S529.5 one release target for all stores: chrome-ext-release grows per-store packaging (Chrome, Edge, Safari) with one version, one changelog, one sha256 manifest'
seq: 2829
status: todo
priority: p2
labels:
    - browser
    - release
created: 2026-10-29T01:59:01.227952Z
---

Extend the S512.3 target instead of adding new ones: same source tree, per-store manifest overlays only where a store requires it (e.g. Safari keys, store-only permission variant from S529.1), reproducible artifacts + sha256 per store, version bumped once and checked against the hub floor, upload steps per store each human-gated (@confirm) with credentials from bashy secret. Test: two runs produce byte-identical artifacts for every store.

Parked 2026-11-25 (operator): Sprint 651 supports Chromium only (Chrome and Edge on Windows and macOS). Unlinked from the sprint; kept as a backlog todo, not done. Chrome and Edge share one Chromium ZIP, so per-store packaging has no second format until a non-Chromium target returns.
