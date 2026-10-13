---
id: a92979c1094f
kind: feature
title: Verify signed embedded runner cache and extraction in Yoke
seq: 2019
status: done
priority: p1
labels:
    - sphere
created: 2026-10-13T16:46:02.111877Z
weave: 236
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T16:53:19.059835Z
closed_by: s436-codex-manager
---

Source qualification: docs/validation/ollama-runner-source-2018.md at Yoke 5e8c4687. In Yoke runner_embed extraction/cache, authenticate final executable bytes to exact signed Bashy release identity and reviewed SHA before executing; reject writable sidecar-only trust and ad-hoc codesign. Preserve existing fail-closed managed-service behavior while signed artifact/pins are absent. Add focused negative tests for tampering, wrong identity and offline cache; do not replace Yoke fork gitlink or create non-security upstream patches. Record the Bashy executable early-dispatch contract for a separate Bashy story. No Dragon native inference/full suite. Worker commits with trailers, no push.
