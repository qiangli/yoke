---
id: 8ff1e95b2c4d
kind: bug
title: 'loom arena PushBase: re-pinning an already-pinned base fails ''already up-to-date'', so arena up cannot resume a partial pin'
seq: 13
status: done
priority: p0
labels:
    - arena
created: 2026-09-30T10:53:15.476339Z
assignee: claude-opus5.5
sprint: 110
sprint_id: 26c12bac-7289-517f-ad46-01e2c117cad7
sprint_title: Revalidate Go 1.27 GNU and POSIX conformance
closed: 2026-09-30T10:58:46.944155Z
closed_by: claude-opus5.5
---

Found 2026-09-30 by the #110 conductor: sprint arena up 110 half-pinned (bashy ok, cloudbox push failed), and every retry then fails with 'already up-to-date' because go-git returns NoErrAlreadyUpToDate as an error. Fix: treat NoErrAlreadyUpToDate as success in loom.ArenaClient.PushBase; red/green in external/loom TestArenaPushBaseAndBundleLocalRepo.
