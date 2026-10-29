---
id: 22fbb342586c
kind: bug
title: 'S525.A5w yoke: interpreter-level containment for executable units on every OS (kernel sandbox optional); static UI needs none'
seq: 2916
status: todo
priority: p0
labels:
    - windows
    - linux
created: 2026-10-29T16:25:12.660379Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Sprint 525, goal a5w (parent 944522763e8f, p0). Operator 2026-10-29: do NOT over-engineer. Shaping must work the same on every OS with zero setup.

Root cause: pkg/webconsole/shaped_candidate.go:~820 requireProductionCandidateContainment calls contain.RequireSandbox, which demands a kernel sandbox (sandbox-exec on darwin, bwrap on linux). sandbox_other.go returns unavailable on Windows, and so does linux without bwrap installed. So EVERY preview and activation is refused there, even a static UI-only change that executes nothing server-side, and the base app's api/ units return 503.

Minimal fix:
(1) Require containment only when the candidate adds or changes executable units: api/ Bash# bodies (api/*.md fences), checks/, or the contract. A static UI change (public/**) needs none and must preview and activate on every OS.
(2) Contain executable Bash# units inside bashy's own interpreter (the api runner and the verifier already run bodies via `bashy -c`), identically on every OS:
- writes only under the data dir;
- loopback-only network;
- scrubbed env (same as contain.go);
- exec limited to bashy builtins/userland, no external binaries;
- the effect cap is honoured.
First check how much the existing @contain/@effects/@guard enforcement in the interpreter (sh fork, yoke dag CapExecHandler, the effect envelope) already gives. Fix the gaps in ONE place, the interpreter-level containment, and select it via contain.go as the baseline level on all OSes.
(3) The kernel sandbox stays as an ADDITIONAL layer where available (darwin sandbox-exec; linux bwrap if present). Its absence must not refuse. When present on linux, bind the resolved runner path read+exec so a bashy in ~/.local/bin works inside it.
No Windows job objects or AppContainer unless the interpreter route is proven insufficient: write down why if so.
Zero setup: the shaping and verify paths use go-git and bashy natives, no host git or go. Fix any exec of host git you find in pkg/shape, pkg/src or pkg/webconsole/shaped*.go.

Acceptance: the A5r denial table runs with only the interpreter containment, i.e. with the kernel layer forced off by a test hook (contain_test.go plus TestShapedProductionGates/{preview_escape,revoked_grant}): cross-user write, BASHY_HOME write, secret env, non-loopback network and external exec are all denied. A static-UI-only candidate previews and activates with no kernel sandbox. Run on Dragon: go test ./pkg/shape/... ./pkg/src ./pkg/webconsole -run 'TestShaped|TestScopeSegment|TestPair' green, plus GOOS=windows and GOOS=linux go vet of the same packages. The manager then runs the same tests natively on a Windows peer and a Linux peer. Keep the change small. Do NOT edit pkg/shape/verify or pkg/shape/evidence semantics, or the protected launcher auth/pairing files. Sprint/Story/Story-ID trailers; no hostnames.
