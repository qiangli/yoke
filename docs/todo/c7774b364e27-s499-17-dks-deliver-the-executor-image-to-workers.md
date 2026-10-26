---
id: c7774b364e27
kind: bug
title: 'S499.17 DKS: deliver the executor image to workers (every job stuck at ImagePullBackOff on localhost/bashy-scratch:<commit>); dks run must report it instead of waiting silently'
seq: 2766
status: todo
priority: p0
created: 2026-10-26T02:35:48.956453Z
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
---

2026-10-25: demo unblocked by hand: bashy-scratch built by bashy on novidesign from v1.0.0-rc.15-dev (make build-bashy-scratch + bashy self image, elfaudit/releaseeligibility PASS) and published public as ghcr.io/qiangli/bashy-scratch:v1.0.0-rc.15-dev (+ :c119fd68); with BASHY_DKS_IMAGE set, a job from Dragon ran on puppy and exited 7. Remaining (lasting fix): the release pipeline publishes this image every release and defaultImagePrefix points at it (yoke external/dks/userland_run.go); dks run/ps surface ErrImagePull instead of waiting.
