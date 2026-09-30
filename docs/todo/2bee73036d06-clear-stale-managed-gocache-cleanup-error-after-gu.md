---
id: 2bee73036d06
kind: bug
title: Clear stale managed GOCACHE cleanup error after guarded run prune succeeds
seq: 15
status: todo
priority: p0
created: 2026-09-30T11:10:28.983093Z
sprint: 319
sprint_id: 8a7c3136-c235-5e11-933f-2cef714109b6
sprint_title: 'Bash# interpreted compiler packages: residual roots after Sprint 281 (R45)'
---

sh#231 remains UNCLEAN on Sprint 319 after its managed GOCACHE directory has disappeared and weave prune has succeeded. weavePruneOwnedRun compacts settled row paths but leaves CleanupError unchanged; sprint prune/end still refuse it. Clear the old error only when the guarded run artifact prune fully succeeds; preserve a real failure. Add a red/green regression and verify Sprint 319 prune no longer reports sh#231.
