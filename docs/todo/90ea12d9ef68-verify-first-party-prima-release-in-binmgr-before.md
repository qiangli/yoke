---
id: 90ea12d9ef68
kind: feature
title: Verify first-party prima release in binmgr before shard execution
seq: 2013
status: done
priority: p2
created: 2026-10-13T13:53:59.96868Z
weave: 229
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T16:36:49.509189Z
resolution: fixed
closed_by: s436-codex-manager
---

Substory of Sprint 436 17ba375e. Source tag bashy-prima-0.1.0 peels to f4ea9845. Bashy CI contract: generated release tag bashy-bashy-prima-0.1.0-1 at qiangli/prima.cpp, assets prima-{linux,darwin}-{amd64,arm64} raw executables, SHA256SUMS, per-asset .sigstore.json. Require exact workflow identity https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/bashy-bashy-prima-0.1.0-1 and OIDC issuer https://token.actions.githubusercontent.com. Extend yoke/pkg/binmgr with a prima tool declaration that verifies committed asset sha256 AND Sigstore bundle against exact identity before returning any executable; reject absent pins/assets/bundle, wrong platform/identity, legacy cache. Add meaningful negative tests. Do not invent SHA pins: keep provision fail closed until hosted CI yields exact assets, then follow up. Worker alone commits with Sprint/Story/Story-ID trailers. Coordinate with S436 conductor before claiming completion; no Dragon heavy builds/containers.
