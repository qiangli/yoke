---
id: 01a17f7c-c9c1-734b-a9e7-2ca2b4ba1b0a
seq: 10
form: page
type: gotcha
title: Gitea's 401 on a push mirror is an address refusal, not auth
description: 'WHEN a Gitea push mirror to a LAN/loopback replica answers 401 Permission denied (POST /repos/{o}/{r}/push_mirrors): it is not a credential problem — HandleRemoteAddressError maps ErrInvalidCloneAddr{IsPermissionDenied} to 401 and a correct all-scoped token fails identically. Applies to bashy loom replica add and any mesh-local replica.'
tags:
    - loom
    - gitea
    - mirror
status: candidate
evidence: S515.4 round 4 live A/B on the Windows peer; docs/s515-4-live-round4.md; external/loom/mirror.go + mirror_test.go
source:
    tool: claude-opus5-p
    episode: ep-e3608a832e4bb601
    parent_episode: ep-265018ce839bbd5f
created: "2026-10-27T22:30:46Z"
---

Set [migrations] ALLOW_LOCALNETWORKS = true on the loom that INITIATES the mirror (the primary); the replica needs nothing. In loom this is the LOOM_MIRROR_ALLOWED_DOMAINS env var, read by loom start, which writes and converges the key in app.ini — no hand-editing.

The trap next to it: ALLOWED_DOMAINS is a whitelist that REPLACES a default. The pinned fork's services/migrations.Init appends its MatchBuiltinExternal allow-rule only while ALLOWED_DOMAINS is empty, so naming any replica host silently stops github.com (and every other external host) from being mirrorable. Always carry github.com,*.github.com in the list. Gitea matches the list against the HOSTNAME only (IsMigrateURLAllowed splits the port off first), so a host:port pattern never matches — strip ports.

Enabling local networks loosens an SSRF guard, which is why it is an explicit per-host opt-in rather than a loom default: on a hosted multi-owner satellite the host's own services listen on loopback.
