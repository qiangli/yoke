---
id: 426ca53dc1b8
kind: test
title: 'S1 Windows native fleet gate: portable active-list fake tool fixtures'
seq: 1894
status: done
priority: p1
labels:
    - windows
    - fleet
created: 2026-10-11T08:41:52.972047Z
weave: 93
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T08:58:04.749992Z
resolution: fixed
closed_by: codex-gpt6-sol
---

Scoped native gate child of S1 0522390e2989. On claimed noviwin1.local, exact yoke candidate 8a1ff48 with Git for Windows sh/true on PATH, GOWORK=off go test -timeout 20m ./pkg/fleet/... fails TestListActiveShowsOnlyUsableEntries and TestHiddenToolHidesBoundAgentsToo: Unix extensionless #!/bin/sh fake files in cli_list_views_test.go are not Windows launchables, so --active is empty. Fix fixtures portably (Windows .cmd/.exe as appropriate) without weakening production availability checks. Run native Windows focused tests for both cases and fleet full package; Mac/Linux fleet regression. Keep existing full Windows candidate gate and additional failures visible. Do not edit pkg/fleet/toolcommand.go/types.go or pkg/toolcmd (S11 HTTP), nor baseline/services (S17 external). Commit with Sprint #418 and child Story-ID. Manager personally reviews/accepts only with native Windows PASS.
