---
id: 7ef25de05004
kind: bug
title: 'AIX canary crossvet: go-billy/v5 osfs undefined unix.Flock'
seq: 2026
status: todo
labels:
    - aix
created: 2026-10-18T16:41:17.909445Z
---

Inherited baseline failure on yoke main 2b4f7a33 (reproduced by Sprint 467 and Sprint 453 crossvet, 2026-10-18): GOOS=aix vet fails compiling github.com/go-git/go-billy/v5 osfs (undefined unix.Flock). Shipping Windows/Linux/Darwin pass. Not caused by 453 storage or 467 weave changes. Fix at the dependency edge (build tag, billy version, or excluding the AIX canary from that import).
