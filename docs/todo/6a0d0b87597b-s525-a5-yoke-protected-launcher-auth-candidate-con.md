---
id: 6a0d0b87597b
kind: feature
title: 'S525.A5 yoke: protected launcher auth + candidate containment'
seq: 2891
status: todo
priority: p1
created: 2026-10-29T07:16:11.410953Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Part of S525.A5 (umbrella story 5c7b49ea6ae9). Enforced shaping security in yoke. Read first: ../docs/sprint-525-a0-decision-record.md sections D-A5 and 5 (threat model) (private; parent dir of this repo; do not copy its text here).
Scope:
- pkg/src/manifest.go ProtectedManifest(): add the launcher auth surface (pkg/webconsole host_auth, pairing, pair_*, login, gate files) and pkg/coopauth; reserve entries for the shaping verifier/evidence store and pkg/shape state code (paths created by later stories; entries are harmless if absent). Every addition carries a recorded reason.
- New candidate containment helper (e.g. pkg/shape/contain or pkg/procguard extension): run a command in an isolated clone dir with cwd-only writes, procguard + pkg/resources limits, @guard effect cap, scrubbed environment (no inherited secrets/credentials; explicit per-principal allowlist only), loopback-only network intent. If a requested containment level is unavailable on the host, return a typed refusal (activation must block), never a silent downgrade.
- Do NOT edit pkg/webconsole non-manifest code (S525.A4) or pkg/skills (S525.A1).
Acceptance (one focused red/green test): denial table — protected path edit refused by src gates for the new entries; candidate cannot read a secret env var or a file outside its clone; unsupported containment level => typed refusal. go test ./pkg/src ./pkg/shape/... green. Behavior-only docs/commits; no hostnames. Sprint/Story/Story-ID trailers.
