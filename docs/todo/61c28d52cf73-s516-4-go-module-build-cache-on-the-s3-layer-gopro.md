---
id: 61c28d52cf73
kind: feature
title: S516.4 Go module + build cache on the S3 layer (GOPROXY to a peer module proxy, GOCACHEPROG to the S3 build cache)
seq: 2779
status: todo
priority: p1
labels:
    - peers
created: 2026-10-27T15:42:36.609582Z
sprint: 516
sprint_id: 79f7749e-6ce2-5fcd-8ae4-5e156e4c6aac
sprint_title: 'heavy services on peers: one S3 layer (seaweedfs) as backing store + LAN cache for ollama models, OCI images, CI/LFS, Go caches; heavy jobs off the dev box'
---

Spec: docs/sprint-516-peer-heavy-services.md (umbrella), story S516.4. KISS/MVP, reuse the pieces listed in the spec; no new subsystem. Heavy jobs on the big peer, never the dev box. No hostnames in code/docs.
