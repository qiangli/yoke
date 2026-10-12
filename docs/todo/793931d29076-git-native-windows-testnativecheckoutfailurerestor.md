---
id: 793931d29076
kind: test
title: 'git (native, Windows): TestNativeCheckoutFailureRestoresOriginalIndex fails - chmod cannot make a directory unwritable on Windows, so the forced checkout failure never happens'
seq: 1905
status: done
labels:
    - windows
    - git
created: 2026-10-12T04:32:20.430691Z
assignee: s9999-gitparity-manager
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T05:35:57.524077Z
closed_by: s9999-gitparity-manager
---

Pre-existing on Windows (verified at a234655~1 and 8b2c14c on noviwin1, Git for Windows 2.55, Go 1.27). Also fails on Linux when run as root (golang:1.27 container); passes as uid 1000. The test needs a platform-neutral way to force the write failure (e.g. a read-only file in place of a directory, or an injected filesystem error).
