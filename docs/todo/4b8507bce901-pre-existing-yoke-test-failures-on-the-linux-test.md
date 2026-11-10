---
id: 4b8507bce901
kind: bug
title: Pre-existing yoke test failures on the Linux test host (Ubuntu WSL, root)
seq: 3085
status: todo
priority: p2
labels:
    - test
    - linux
created: 2026-11-10T17:01:12.283253Z
---

Found by the S606/S583 final suite run 2026-11-10 (yoke 63682cd7) and confirmed on the pre-sprint baseline 411b219d on the same host (GOWORK=off, go1.27.2 linux/amd64, root in WSL): cmds/all build (gpgme cgo, no gcc), pkg/atlas TestCensusCoversEveryPackage, pkg/dag TestCommandFleetRunsCommittedChunks, pkg/dbobj TestIndependentProcess_SkippedInShort, pkg/indexer TestTemporaryRootsAlwaysExcluded, pkg/telemetry TestStartupNoticeNamesAnchoredSpoolPath; flaky: pkg/broker TestAuditFileCarriesCostFields (2/8 on baseline, 3/8 on candidate: audit file 1 priced, 0 unpriced), pkg/chat TestDelegateVendorContract (fails only under full-package load). Not regressions from S606/S583; track and fix separately (likely root/WSL temp-path assumptions).
