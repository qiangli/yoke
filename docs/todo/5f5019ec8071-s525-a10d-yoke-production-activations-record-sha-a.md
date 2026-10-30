---
id: 5f5019ec8071
kind: bug
title: 'S525.A10d yoke: production activations record SHA as request, author unknown, time 1970, so history can''t say who/when/what'
seq: 2923
status: done
priority: p0
labels:
    - software-shaping
created: 2026-10-29T17:16:52.127115Z
weave: 708
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T17:29:47.931054Z
resolution: fixed
closed_by: muse-conductor-f
---

Sprint 525, goal a10 (parent 671de6e3). Bug found by the live A9 smoke (Linux, elephant WSL, installed candidate bashy c7680732 / yoke e2842bb5, real genie shaping via the web console). After two real activations through the production path (POST /api/shape/notes/candidates/{id}/approve), `bashy src history notes --user device:alice --json` (and GET /api/shape/notes/history) lists each version with:
  "request": "<the revision SHA>"   (not the user's request text)
  "author":  "unknown"
  "when":    "1970-01-01T00:00:00Z"
A10 requires users to see who, when and what was asked for every version. The unit tests pass because they record those fields themselves; the production approve path (pkg/webconsole/shaped_candidate.go productionActivator -> src.Activate) does not pass the candidate's request text, principal and time into the activation record/commit, and history then falls back to the SHA.

Fix: the production activation (approve) and restore/undo record request = the candidate's request text (restore: "restore to <name or short sha>"; undo: "undo"), author = the principal, when = the activation time (the server clock, UTC), in the activation commit the history reads. Same through the CLI (`src restore/undo`, `src activate`). Do not change the evidence binding.

Acceptance: a focused test through the REAL production approve handler (not a test-only activator) that asks, approves, then reads history. It asserts the request text, the principal and a recent timestamp; a restore shows its own request line. go test ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair' ./pkg/src ./pkg/shape/... green. Avoid shaped_delegate.go/shaped.go runner defaults (S525.A9d in parallel). Sprint/Story/Story-ID trailers; no hostnames.
