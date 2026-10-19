---
id: 46787d701f6b
kind: bug
title: Include signed Prima no-egress evidence rows in verifier allowlist
seq: 2016
status: done
priority: p1
created: 2026-10-13T14:30:47.182676Z
weave: 233
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T16:36:56.567421Z
closed_by: s436-codex-manager
---

Correction to Yoke signed sums allowlist c07119b4. Bashy builds/prima.cpp/fork/.github/workflows/bashy-build.yml uploads out/* from each successful native build. The no-egress gate creates out/prima-<os>-<arch>-egress.txt, so release SHA256SUMS necessarily includes four signed egress evidence rows. Current primaSupplementarySumsNames omits -egress.txt and would reject a valid release. Add that exact suffix for four platforms; test complete realistic manifest and rejection of unknown unsafe extras. Keep exact identity/issuer, independent binary SHA pin, duplicate/traversal checks and offline cache verification. Worker alone commits with Sprint/Story/Story-ID trailers and story file; no push or dev-host native build/container. No SHA pins until hosted CI signs assets.
