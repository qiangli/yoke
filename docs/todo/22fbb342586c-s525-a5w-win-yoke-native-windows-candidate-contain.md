---
id: 22fbb342586c
kind: bug
title: 'S525.A5w-win yoke: native Windows candidate containment (AppContainer/restricted token + Job Object), same denial table, zero setup'
seq: 2916
status: todo
priority: p0
labels:
    - windows
created: 2026-10-29T16:25:12.660379Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Sprint 525, goal a5w (parent 944522763e8f, p0, security-critical). Operator: shaping is a bashy feature; it must behave identically on every OS, and any platform difference or refusal is a bug. Today pkg/shape/contain/sandbox_other.go makes native Windows refuse candidate containment ("no kernel sandbox backend on this platform"), so shaped-app preview, activation, history preview, and even the base app's api/ units return 503 on Windows.

Build a native Windows backend in pkg/shape/contain (sandbox_windows.go plus helpers) with the SAME guarantees and the SAME denial table as darwin (sandbox_darwin.go) and linux (sandbox_linux.go):
- writes only inside the candidate/data dir; no reads outside the clone/data dir and the runner's own read+exec path;
- no secret or console env (same env shaping as contain.go);
- network loopback-only, or none, matching the other backends;
- the effect cap is honoured; resource limits (CPU time, memory, process count) and kill-on-close via a Job Object (kill_windows.go already exists);
- use OS primitives: AppContainer or a restricted token with low integrity plus a Job Object, ACLs granted to the container SID on the allowed dirs only, and a firewall/AppContainer capability for network denial. Pure Go via golang.org/x/sys/windows; no third-party binaries, no admin rights, no user setup.
- The runner (bashy.exe) may live anywhere (e.g. %LOCALAPPDATA%\outpost). Grant read+exec on the resolved runner path only.
- RequireSandbox and the probe must pass on a stock Windows 11 user account with zero setup.

Zero-setup toolchain: confirm the shaping and verify paths need no host git or go (go-git and bashy natives). Fix any exec of host git in pkg/shape, pkg/src or pkg/webconsole/shaped*.go.

Acceptance: the existing contain denial table (contain_test.go plus TestShapedProductionGates/preview_escape) passes with GOOS=windows on a real Windows host, with no skips added for Windows. Run on Dragon: GOOS=windows go vet ./pkg/shape/... ./pkg/webconsole and go test ./pkg/shape/... ./pkg/src (darwin must stay green). The manager runs the native Windows test on a Windows peer. Make the tests self-checking (they assert each denial, not just exit codes). Do NOT edit pkg/shape/verify or pkg/shape/evidence semantics, or the protected launcher auth/pairing files. sandbox_linux.go is being changed by a parallel story (S525.A5w-linux): stay out of it, and coordinate any change to contain.go through small, additive helpers. Sprint/Story/Story-ID trailers; no hostnames.
