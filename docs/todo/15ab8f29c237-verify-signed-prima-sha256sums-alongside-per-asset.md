---
id: 15ab8f29c237
kind: feature
title: Verify signed Prima SHA256SUMS alongside per-asset bundle
seq: 2014
status: done
priority: p1
created: 2026-10-13T14:12:28.96356Z
weave: 231
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T14:23:30.284824Z
resolution: fixed
closed_by: codex-gpt6-sol-w
---

Substory of 17ba375e and the existing Yoke binmgr Prima verifier. For a candidate release, download SHA256SUMS and SHA256SUMS.sigstore.json, verify the sums file with cosign against exact release-tag workflow identity and OIDC issuer, parse strictly (no duplicate, path traversal, malformed, or extra asset rows), and require its Prima asset row to equal both downloaded binary SHA and independently committed pin before execution. Preserve signed sums and bundle for offline cache reuse; reverify them on cache hits. Keep fail-closed while hosted CI has no released assets or committed SHA pins. Add meaningful negative/offline tests. Worker alone commits with Sprint/Story/Story-ID trailers and story file; no Dragon native build/container and no push.
