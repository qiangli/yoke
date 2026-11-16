---
id: a5447d9015dc
kind: bug
title: crossvet red on windows + linux at main (podman fork unix.Rlimit; gpgme build constraints)
seq: 3116
status: todo
priority: p2
labels:
    - crossvet
created: 2026-11-16T12:18:50.497741Z
---

Found 2026-11-16 while gating S529.1; reproduced on unchanged origin/main 8ee6c331 in a clean worktree on the macOS dev box (GOWORK=off scripts/crossvet.sh). windows: external/podman/src/pkg/util/rlimit.go undefined unix.Rlimit/Getrlimit. linux: github.com/proglottis/gpgme@v0.1.6 build constraints exclude all Go files (cgo-only package under a cgo-off cross vet). aix is tracked separately (7ef25de0). darwin passed on the candidate and failed once on the baseline worktree; recheck before trusting either. Root cause not yet identified: when did external/podman enter the crossvet scope, and is it meant to (CLAUDE.md says the CI scope excludes external/)? Classify and record before any fix.
