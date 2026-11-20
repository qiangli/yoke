---
id: 6ae59bf83233
kind: refactor
title: S575 reference-only consumers of unchanged Bash source
seq: 3127
status: done
priority: p0
created: 2026-11-18T04:09:09.151765Z
assignee: s575-codex-manager
sprint: 575
sprint_id: 65ce85f5-87c0-5863-bf55-62de86f5df36
sprint_title: 'Layer separation: Classic <- Bash# <- Yoke import-graph delineation'
closed: 2026-11-20T12:26:28.227164Z
closed_by: s575-codex-manager
---

Operator correction: move repository source first; defer mixed sh internal refactoring. Only rewrite old engine imports/module references to the unchanged qiangli/bash source. Preserve every implementation/test body. Build with GOWORK=off in locked official image.

## Manager closeout — 2026-11-20
Engine import/module reference cutover published at 79fe8f90/8bdfc009. Retained official-image module build and owned-package compilation qualify this reference-only scope. Existing unrelated database/external-venue failures were separately tracked; this is not a full current-head Yoke-suite pass.
Existing receipts and source ancestry were reviewed; no new test/build ran during reconciliation. Evidence belongs to its exact historical source/artifact identities, not arbitrary later heads. S575 full-run evidence remains under posix-baseline/s575-2026-11-19 with its raw 4 FAIL, 54 UNRESOLVED and 9 INSPECT unchanged. RC38 is on operator hold.
