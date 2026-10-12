---
id: 854ad5d1f40d
kind: task
title: 'yoke pkg/atlas red on main: pkg/unit has no census entry; seaweedfs windows asset citation mismatch'
seq: 1907
status: done
created: 2026-10-12T06:53:05.13533Z
weave: 185
assignee: agy-gemini3.8-flash
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T14:54:31.999619Z
closed_by: s9999-maintenance-manager
---

Found by Sprint 422 merge gate. Both fail on yoke 610b7af BEFORE the S422 feed merge (verified in a detached worktree): TestCensusCoversEveryPackage (pkg/unit not classified in pkg/atlas/packages.go) and TestWindowsAssetCitationsMatchTheResolvers (seaweedfs cited windows_amd64.tar.gz is not what its resolver picks). Owners: pkg/unit author (S420 unit envelope) and the seaweedfs external registry. Verify: go test ./pkg/atlas/.

S9999 lane: atlas pkg/unit census and SeaweedFS Windows citation. Keep assertions strict. Focused go test ./pkg/atlas. No full builds/containers on Dragon; isolated submodule commit, no push; send SHA/tests to s9999-maintenance-manager.
