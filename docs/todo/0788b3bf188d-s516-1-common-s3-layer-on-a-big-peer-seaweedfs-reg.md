---
id: 0788b3bf188d
kind: feature
title: 'S516.1 common S3 layer on a big peer: seaweedfs registered with the coordinator as the LAN store, reached by the 515.2 stable name'
seq: 2776
status: todo
priority: p0
labels:
    - peers
created: 2026-10-27T15:42:36.262412Z
sprint: 516
sprint_id: 79f7749e-6ce2-5fcd-8ae4-5e156e4c6aac
sprint_title: 'heavy services on peers: one S3 layer (seaweedfs) as backing store + LAN cache for ollama models, OCI images, CI/LFS, Go caches; heavy jobs off the dev box'
---

Spec: docs/sprint-516-peer-heavy-services.md (umbrella), story S516.1. KISS/MVP, reuse the pieces listed in the spec; no new subsystem. Heavy jobs on the big peer, never the dev box. No hostnames in code/docs.
