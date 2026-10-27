---
id: e81d03f2710b
kind: feature
title: 'S516.3 OCI images local first: bashy oci pull checks peers (zot pull-through on the S3 layer; LAN then mesh) before the registry, publishes what it pulls'
seq: 2778
status: todo
priority: p0
labels:
    - peers
created: 2026-10-27T15:42:36.495488Z
sprint: 516
sprint_id: 79f7749e-6ce2-5fcd-8ae4-5e156e4c6aac
sprint_title: 'heavy services on peers: one S3 layer (seaweedfs) as backing store + LAN cache for ollama models, OCI images, CI/LFS, Go caches; heavy jobs off the dev box'
---

Spec: docs/sprint-516-peer-heavy-services.md (umbrella), story S516.3. KISS/MVP, reuse the pieces listed in the spec; no new subsystem. Heavy jobs on the big peer, never the dev box. No hostnames in code/docs.
