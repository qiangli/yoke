---
id: 01a16cee-4307-768b-9239-561d177de116
seq: 7
form: page
type: gotcha
title: Prune dry runs must use read-only room membership
description: WHEN building a cleanup preview, room.Members reconciles stale cards and creates the members directory, so it mutates state. Use room.PeekMembers for candidate scans and recheck under the member-claims guard before applying deletion.
status: candidate
source:
    tool: codex-gpt6-sol-t
    episode: ep-d124fa6191d4526a
    parent_episode: ep-d62d5ebffb33162f
created: "2026-10-24T08:01:56Z"
---
