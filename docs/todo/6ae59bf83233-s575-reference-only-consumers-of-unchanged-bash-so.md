---
id: 6ae59bf83233
kind: refactor
title: S575 reference-only consumers of unchanged Bash source
seq: 3127
status: assigned
priority: p0
created: 2026-11-18T04:09:09.151765Z
assignee: s575-codex-manager
sprint: 575
sprint_id: 65ce85f5-87c0-5863-bf55-62de86f5df36
sprint_title: 'Layer separation: Classic <- Bash# <- Yoke import-graph delineation'
---

Operator correction: move repository source first; defer mixed sh internal refactoring. Only rewrite old engine imports/module references to the unchanged qiangli/bash source. Preserve every implementation/test body. Build with GOWORK=off in locked official image.
