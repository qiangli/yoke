---
id: 3d27603dc63e
kind: feature
title: 'S525.A7 yoke: per-user versions, atomic activation, rollback'
seq: 2894
status: todo
priority: p1
created: 2026-10-29T07:44:57.252178Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Part of S525.A7 (umbrella story a48023916878). Per-user versions, atomic activation and rollback on the local Git/Weave path, in yoke. Read first: ../docs/sprint-525-a0-decision-record.md sections D-A7/A10 and D-MVP (private; parent dir of this repo; do NOT copy its text or describe the internal mechanism in public docs/commits — describe behavior only).
Already on main: pkg/shape (A4 per-principal store + feature metadata), pkg/shape/contain (A5), pkg/webconsole principal resolution (A4), unified unit model (A1). S525.A2 (verifier + evidence gate in pkg/shape/verify, pkg/shape/evidence, pkg/src/pr.go) is running in parallel.
Scope:
1. Housekeeping first: move the A4 state code (pkg/shape/store.go, pkg/shape/feature.go) into package pkg/shape/state (that path is reserved protected) and update callers; add pkg/webconsole/principal.go to src.ProtectedManifest() with a Reason.
2. Per-user version line inside the app src checkout: one line per principal, named versions and review versions under the same per-principal namespace (exact ref layout per the decision record). Every activation records a commit on that line with request/verdict/evidence/principal trailers.
3. Activation: call an ActivationGate interface (define it in pkg/shape/state; A2 will satisfy it — use a fake in tests), then switch the principal active revision atomically; keep last-known-good; a crash mid-activation leaves the previous active revision. Concurrent activations by two principals never touch each other.
4. Preview: weave can start an isolated workspace at an explicit base commit (add a --base <sha> option); active revision untouched.
5. Upgrade: src upgrade replays every principal line onto the new release; on conflict it aborts losslessly (existing behaviour) and records a review version for that principal; others proceed.
6. CLI under `bashy src` (yoke src cmd): activate <app> <rev>. (history/preview/diff/name/restore are S525.A10, next story — leave seams.)
- Do NOT edit pkg/src/pr.go, pkg/src/approval.go, pkg/shape/verify, pkg/shape/evidence (A2).
Acceptance (one focused red/green test on a temp src store): interrupted activation keeps previous; two principals activate concurrently without interference; upgrade conflict for one principal => review version, other principal upgraded; data dir untouched throughout. go test ./pkg/src ./pkg/shape/... ./pkg/weave -run <your tests> green. Sprint/Story/Story-ID trailers; no hostnames.
