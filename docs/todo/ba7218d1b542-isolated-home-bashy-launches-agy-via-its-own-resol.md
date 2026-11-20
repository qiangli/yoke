---
id: ba7218d1b542
kind: bug
title: 'Isolated HOME: bashy launches agy via its own resolution and agy exits instantly with no error'
seq: 3135
status: done
priority: p2
labels:
    - agents
created: 2026-11-20T09:27:58.977737Z
assignee: claude-opus5.5-s676
sprint: 676
sprint_id: cc17f40d-d5ff-5d27-bbed-b8563759cde6
sprint_title: 'Fleet hardening after the tool-shortcut tests: steerable ycode/zcode/dsh + pre-existing test failures'
closed: 2026-11-20T16:01:40.944973Z
closed_by: claude-opus5.5-s676
---

Sprint 674 (2026-11-20): with an agent-bench arm HOME (fresh BASHY_HOME/XDG cache) and no BASHY_TOOL_BINARY_AGY, bashy chat --agent agy-gemini4-carbon -i exits 'before its instruction was delivered' within ~100 ms and the session log is empty; nothing is installed into the arm cache. Pinning the same 1.5.13 binary (BASHY_TOOL_BINARY_AGY / arm_home --binary) launches fine. Bumping the managed pin to 1.5.13 (11b02e37) did not change it. Root-cause how bashy resolves the agy executable when the managed copy is absent in a fresh HOME (managed path vs PATH vs install), and make the failure explicit (name the resolved path and the exec error) instead of a silent exit.
