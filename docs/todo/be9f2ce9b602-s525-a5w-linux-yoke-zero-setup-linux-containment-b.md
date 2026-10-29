---
id: be9f2ce9b602
kind: bug
title: 'S525.A5w-linux yoke: zero-setup Linux containment (bashy-owned namespaces, no host bwrap, runner anywhere)'
seq: 2917
status: todo
priority: p0
labels:
    - linux
created: 2026-10-29T16:25:12.881454Z
sprint: 525
sprint_id: 9a9a4443-02c8-53a3-b0bd-96bd67980643
sprint_title: 'Software Shaping foundation: verified personal features and same-host promotion'
---

Sprint 525, goal a5w (parent 944522763e8f, p0, security-critical). Operator: shaping must need zero setup on every OS; any manual host step is a bug. Today pkg/shape/contain/sandbox_linux.go shells out to bwrap. On a stock Ubuntu (WSL2 or server) without bubblewrap, RequireSandbox refuses ("bwrap not found"), so shaped preview, activation, history preview and even the base app's api/ units return 503. Also, only /usr, /bin, /lib and /lib64 are bound, so a bashy runner installed in ~/.local/bin (bashy's default install dir) fails inside the sandbox with 500 "unit list failed (exit 1)".

Replace the bwrap dependency with a bashy-owned Linux sandbox. Re-exec the bashy binary as a contain helper that uses unprivileged user + mount + pid + net (+ ipc/uts) namespaces via syscall clone flags, plus pivot_root/bind mounts. Add Landlock and seccomp where the kernel supports them, but the guarantees must hold even without Landlock. Keep the SAME guarantees and denial table:
- writes only inside the candidate/data dir; no reads outside the clone/data dir, the system lib dirs and the resolved runner path;
- no secret or console env;
- no network, or loopback-only, matching darwin;
- effect cap, resource limits, kill on parent death.
The runner may live anywhere: bind its resolved path read+exec. No host binaries required beyond the kernel; no root.
If unprivileged user namespaces are disabled by the kernel or sysctl (e.g. Ubuntu apparmor_restrict_unprivileged_userns), print a precise diagnostic AND fall back to an equally strong mechanism if one exists (Landlock + seccomp + an empty net ns via an existing helper). Never silently weaken containment.

Acceptance: contain_test.go denial table plus go test ./pkg/webconsole -run 'TestShaped' green on Linux WITHOUT bwrap installed and with the runner under $HOME/.local/bin. darwin stays green. Run Linux tests where you can (GOOS=linux vet on Dragon; the manager runs the native Linux test on a WSL2 Ubuntu peer). Do NOT edit sandbox_darwin.go semantics, pkg/shape/verify or pkg/shape/evidence, or the protected auth/pairing files. sandbox_windows.go is being built by a parallel story (S525.A5w-win): stay out of it, and keep contain.go changes small and additive. Unit tests only on Dragon; no podman. Sprint/Story/Story-ID trailers; no hostnames.
