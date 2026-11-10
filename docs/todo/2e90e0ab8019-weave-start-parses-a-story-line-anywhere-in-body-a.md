---
id: 2e90e0ab8019
kind: bug
title: weave start parses a 'Story:' line anywhere in --body as a trailer
seq: 3082
status: todo
priority: p2
labels:
    - weave
    - sprint
created: 2026-11-10T12:01:22.560565Z
---

A run body containing a heading line 'Story:' (before the real trailer block) failed the worker claim: 'explicit story trailers: each Story trailer needs one matching Story-ID trailer (got 2 Story, 1 Story-ID)'. Expected: only the final trailer block (git interpret-trailers semantics) counts.
