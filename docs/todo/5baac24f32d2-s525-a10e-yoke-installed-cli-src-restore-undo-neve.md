---
id: 5baac24f32d2
kind: bug
title: 'S525.A10e yoke: installed CLI src restore/undo never verify (refuse on real data), take activation ids; CLI history JSON key differs from web'
seq: 2925
status: done
priority: p0
labels:
    - software-shaping
created: 2026-10-29T17:40:17.893191Z
weave: 710
assignee: muse-conductor
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T17:55:10.967004Z
resolution: fixed
closed_by: muse-conductor-h
---

Sprint 525, goal a10 (parent 671de6e3), p0. Bug found by the full live A9 run (Linux, installed candidate bashy b044891d / yoke 583e4c7f). Web history, restore and undo all pass live. The installed CLI does not.

`bashy src restore notes <id> --user device:alice` on a real history (3 approved versions, a web restore and undo) refuses:
  src: restore refused: gate: refusing to activate f1d980e9b807: evidence: no record for this candidate: f1d980e9
(1) The web restore route (shaped_history.go) first verifies the target against the current tip through the trusted verifier, sealing fresh evidence for exactly that (candidate, base, principal, verifier) binding, and only then commits forward through the gate. The CLI restore and undo (A10c) only CHECK the gate, and there is never a sealed record for a restore pair, so real CLI restores always refuse. TestS525A10ProductionGateCLI passed only because it pre-sealed evidence.
(2) The CLI passed the ACTIVATION commit id (from `src history`) as the candidate. History lists both activation and revision; restore must accept either and resolve to the version's revision.
(3) `src history --json` names the list `entries`, while the web GET /api/shape/<app>/history names it `versions`, with the same element fields. Make the CLI JSON use `versions` (keep `entries` as an alias for one release), so the app view and the CLI are the same history view.

Fix: one shared restore/undo function, used by both the web route and the src CLI (pkg/src or pkg/shape), that verifies, seals and gates. Same refusals: schema-incompatible, verifier fail, gate fail. Identical on every OS.

Acceptance: a focused test using the REAL production path (no pre-sealed evidence). Ask/approve 3 versions via the production approve handler, then `src restore <app> <activation id> --user P` succeeds and moves active, `src undo` returns, and `src history --json` has .versions with request/author/when. Schema-incompatible restore refuses with the reason. go test ./pkg/src ./pkg/shape/... ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair|TestS525' green; GOOS=windows vet. Avoid pkg/procguard and shaped_delegate.go (S525.A9w in parallel). Sprint/Story/Story-ID trailers; no hostnames.
