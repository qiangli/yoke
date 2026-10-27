---
id: 01a17eb4-b456-702a-82d4-cf3dde436c72
seq: 8
form: page
type: gotcha
title: Verify localhost resolution in the actual storage client
description: When using peer.satellite.localhost for signed S3 URLs, a successful curl or browser request does not prove Go client resolution. S515.2 Windows Go 1.27.2 Gitea failed with no such host while outpost routing worked. Open SeaweedFS 4.48 also rejected signed writes with unconfigured keys; anonymous downloads proxied through loom. Gate the actual Gitea/client transport and process TCP counters; see docs/s515-2-live-round2.md.
status: candidate
source:
    tool: codex-gpt6-astra-i
    episode: ep-39e0bda486bdda46
    parent_episode: ep-265018ce839bbd5f
created: "2026-10-27T18:52:14Z"
---
