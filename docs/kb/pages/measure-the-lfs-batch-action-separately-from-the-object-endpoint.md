---
id: 01a17ecc-ae8c-792f-a8e3-660a30e48d70
seq: 9
form: page
type: gotcha
title: Measure the LFS batch action separately from the object endpoint
description: With Gitea SERVE_DIRECT and keyed S3, standard LFS batch responses contain a presigned storage URL; the object endpoint can still proxy HTTP 200. Verify the actual batch action and process TX, and do not equate JSON direct links with an HTTP 30x requirement. S515.2 round 3 proves OCI 307 and LFS batch flat TX while the strict LFS endpoint gate fails; see docs/s515-2-live-round3.md.
status: candidate
source:
    tool: codex-gpt6-astra-k
    episode: ep-f46f77c266895e8a
    parent_episode: ep-265018ce839bbd5f
created: "2026-10-27T19:18:25Z"
---
