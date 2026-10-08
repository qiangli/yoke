---
id: 84d40c38fa73
kind: bug
title: 'genie: the command reviewer rejects plain compound workspace commands (''pwd; echo x > f; cat f''), forcing retries'
seq: 26
status: todo
priority: p2
labels:
    - genie
created: 2026-09-30T16:44:43.297383Z
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
---

Steward 2026-09-30 after 248e8113 landed: in a fresh scratch git repo, 'bashy genie -m door-codex-gpt-5.5 "...run exactly: pwd; echo steward-ok > f.txt; cat f.txt"' answered 'The command was rejected by the reviewer and did not run.'; the single-step request (create f.txt) passed. The #340 conductor saw the same ('first attempt held by preflight policy'). A workspace-local write+read is the most basic coding-agent step; the policy should allow it within the writable root. Fix (KISS): classify ';'-joined simple commands by their parts (all within workspace-read/write/exec) instead of refusing the compound. Red/green: the compound above runs; a compound touching outside the workspace is still refused.
