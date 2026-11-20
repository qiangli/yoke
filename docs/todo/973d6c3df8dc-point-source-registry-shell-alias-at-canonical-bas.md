---
id: 973d6c3df8dc
kind: task
title: Point source registry shell alias at canonical Bash repository
seq: 3128
status: done
created: 2026-11-18T05:28:38.063277Z
assignee: s575-codex-manager
sprint: 575
sprint_id: 65ce85f5-87c0-5863-bf55-62de86f5df36
sprint_title: 'Layer separation: Classic <- Bash# <- Yoke import-graph delineation'
closed: 2026-11-20T12:26:27.549028Z
closed_by: s575-codex-manager
---

Reference metadata only: preserve the external sh repository key and every classification/assertion; point its canonical remote and any old runtime module references to github.com/qiangli/bash. No engine, database, external dependency, or feature changes. Manager performs focused verification in the official image.

## Manager closeout — 2026-11-20
Canonical source registry reference published at ec36645f. External protocol key sh is preserved. Retained official pkg/src regression evidence passed in 9.640 seconds; no source-engine or feature redesign is accepted here.
Existing receipts and source ancestry were reviewed; no new test/build ran during reconciliation. Evidence belongs to its exact historical source/artifact identities, not arbitrary later heads. S575 full-run evidence remains under posix-baseline/s575-2026-11-19 with its raw 4 FAIL, 54 UNRESOLVED and 9 INSPECT unchanged. RC38 is on operator hold.
