---
id: 7ebab03115a3
kind: bug
title: Accept complete signed Prima release manifest in binmgr
seq: 2015
status: done
priority: p1
created: 0001-01-01T00:00:00Z
weave: 232
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
closed: 2026-10-13T14:29:21.057028Z
resolution: fixed
closed_by: codex-gpt6-sol-x
---

Correction to signed sums worker 421790eb / Story 15ab8f29c237. Bashy CI
hashes every dist file in SHA256SUMS, including four Prima executables, audit
reports, notices, linkage, CMake and input evidence, PIN, dependencies and
SBOM. Accept only those named supplementary rows with strict sha256sum syntax
and unique safe basenames. Require all four executable rows; compare the
selected row with downloaded bytes and the independent committed pin. Verify
the sums signature against the exact workflow identity and issuer, including
offline cache reuse. Exercise realistic complete manifests and negative cases.
Do not add release pins until hosted CI succeeds. No dev-host native build or
container, and no push.
