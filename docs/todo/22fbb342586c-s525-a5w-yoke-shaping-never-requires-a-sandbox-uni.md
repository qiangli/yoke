---
id: 22fbb342586c
kind: bug
title: 'S525.A5w yoke: shaping never requires a sandbox; units run under bashy guards; explicit sandbox islands route to bashy oci; retire contain backends'
seq: 2916
status: done
priority: p0
labels:
    - windows
    - linux
created: 2026-10-29T16:25:12.660379Z
weave: 702
assignee: claude-opus5.5
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
closed: 2026-10-29T16:54:00.689613Z
resolution: fixed
closed_by: claude-opus5.5-z
---

Sprint 525, goal a5w (parent 944522763e8f, p0). OPERATOR 2026-10-29, FINAL; supersedes both earlier A5w designs, which were wrong. Full scope: umbrella docs/todo/944522763e8f-* (dhnt dev f8e305a3d) and docs/software-shaping-epic.md.

(1) Shaping must NEVER require a sandbox, whatever the app.
- Remove the RequireSandbox gate (pkg/webconsole/shaped_candidate.go ~820, requireProductionCandidateContainment) and the default LevelSandbox in shaped.go runUnit and in pkg/shape/verify.
- Candidate and app units run as ordinary Bash# under bashy, governed by bashy's normal guards and effect envelopes (@effects/@guard, the effect cap, the scrubbed env).
- If an app wants isolation, it declares it in its own Bash# through the island features (fences/decorators), and bashy handles it. The app never sees OS details.

(2) An explicit sandbox request routes to bashy oci/sandbox, which works out of the box on Windows, macOS and Linux.
- Retire pkg/shape/contain's custom sandbox-exec/bwrap backends (sandbox_darwin.go, sandbox_linux.go, sandbox_other.go, the RequireSandbox probe).
- Keep only what is NOT a sandbox: scrubbed env, the effect cap, kill/timeout and resource limits as bashy guards.
- If bashy oci/sandbox fails on a supported platform, FILE it as its own bug. Do not work around it in shaping.

(3) KEEP: verifier and evidence outside candidate reach (A2 protected paths, frozen checks, evidence binding); per-user data separation (A4: each principal's own data dir, other users' data never reachable through the app routes); secret-free env and effect caps.

Tests:
- Update TestShapedProductionGates and the contain tests to the new model. Delete assertions that only a kernel sandbox could provide.
- Keep and assert what bashy guards provide: scrubbed env, effect-cap refusal, per-user data separation through the routes, revoked grant, frozen check, missing evidence.
- Add: a static UI change AND a Bash# api unit both preview, verify and activate with NO sandbox available (no bwrap, no sandbox-exec, no podman), the same on every OS.
- Add: an app that declares a sandbox island is routed to bashy oci. A unit test with a fake oci runner is fine; Dragon has no podman, so do not run podman here.
- Gate: go test ./pkg/shape/... ./pkg/src ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair' green, plus GOOS=windows and GOOS=linux go vet of the same packages.
Do NOT edit pkg/shape/evidence semantics or the protected launcher auth/pairing files. A10c (yoke#700) touches the src activation gate wiring in parallel: avoid pkg/src/activate.go. Sprint/Story/Story-ID trailers; no hostnames.
