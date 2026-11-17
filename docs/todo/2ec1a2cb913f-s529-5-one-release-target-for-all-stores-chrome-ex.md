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
sprint: 651
sprint_id: 9f9ec0c8-1df1-518f-9843-991ce50f6ee4
sprint_title: 'bashy live extension: store distribution + cross-browser (Chrome Web Store, Edge Add-ons/Windows, Safari, one release target)'
---

Extend the S512.3 target instead of adding new ones: same source tree, per-store manifest overlays only where a store requires it (e.g. Safari keys, store-only permission variant from S529.1), reproducible artifacts + sha256 per store, version bumped once and checked against the hub floor, upload steps per store each human-gated (@confirm) with credentials from bashy secret. Test: two runs produce byte-identical artifacts for every store.
