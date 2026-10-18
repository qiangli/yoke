---
id: d6f0d15a9bf6
kind: bug
title: sprint reassign cannot resolve a story whose run lives in another repo
seq: 2029
status: todo
labels:
    - sprint
created: 2026-10-18T20:38:11.416458Z
---

Observed 2026-10-18 (Sprint 446): bashy sprint reassign 446 --run agent-bench#8 --override --reason ... --dry-run failed: no issue 2b8bda69888c in the register. The story record lives in the umbrella while the weave run is in agent-bench (cross-repo story). Workaround used: sprint claim 446 2b8bda69888c --owner <agent> --force, then weave handoff. Expected: reassign resolves the story through the sprint's tracked repos like claim does. Fix with a cross-repo regression test.
