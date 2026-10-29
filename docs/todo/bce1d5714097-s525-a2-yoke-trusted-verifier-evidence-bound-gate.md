---
id: bce1d5714097
kind: feature
title: 'S525.A2 yoke: trusted verifier + evidence-bound gate'
seq: 2892
status: todo
priority: p1
created: 2026-10-29T07:27:22.495342Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Part of S525.A2 (umbrella story 6bd2374295a2). Trusted verifier and evidence-bound merge/activation gate in yoke. Read first: ../docs/sprint-525-a0-decision-record.md section D-A2 and section 5 (private; parent dir of this repo; do not copy its text here).
Scope (this run):
- pkg/shape/verify: trusted runner. Checks are read from the BASE commit (never the candidate); a candidate that adds checks gets them run in addition, never instead. Run in an isolated clone detached at the candidate SHA using pkg/shape/contain (already on main). Every required check yields pass/fail/skip/timeout/cancel; only all-pass passes. Contract input via a small interface (ContractReader) so the S525.A1 unit contract reader plugs in later; ship a minimal reader for @ensure/@require now.
- Call accounting: hook the interpreter decorator path so each contracted function invocation is recorded; a contracted function never invoked by the check set fails the verdict.
- pkg/shape/evidence: record written only by the verifier under $BASHY_HOME/src/.evidence/<candidate-sha>.json, outside any candidate tree, sealed (HMAC) with a host key from the existing secrets facility. Binding = candidate SHA, base SHA, verifier build ID, contract digest, dependency digest, principal. Any mismatch = stale.
- pkg/src: pr merge requires a matching passing evidence record; test runs at HeadSHA in an isolated clone (not the live checkout); missing/failed/skipped/stale/wrong-candidate evidence blocks merge; only override is an explicit approval record (existing approval flow), never silent. Core/protected rules unchanged. Expose a Gate(candidate, base, principal) check activation can call.
- Do NOT edit pkg/skills, pkg/craft, pkg/lexicon, pkg/unit (S525.A1 running) or pkg/webconsole, pkg/shape/state (S525.A4 running). The pkg/skills bashsharp repair overlay moves onto this verifier in a follow-up after A1.
Acceptance (one focused red/green table test): early exit, uncalled decorated function, changed binding, forged/edited evidence, edited checks, effect-cap escalation, missing evidence, stale evidence after rebase => rejected; a genuine fix => accepted and merge allowed. go test ./pkg/src ./pkg/shape/... green. Behavior-only docs/commits; no hostnames. Sprint/Story/Story-ID trailers.
