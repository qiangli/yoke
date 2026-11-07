---
id: 9017055a12ef
kind: bug
title: 'S584.C2 unsafe-launch guard: multi-token flags (zcode --mode yolo); muse/zcode canonical unsafe flags'
seq: 3053
status: done
priority: p0
labels:
    - agent-cli
created: 2026-11-07T03:48:25.07947Z
assignee: claude-opus5.5-s584
sprint: 584
sprint_id: d1c8cf7b-5ef0-566c-9a2e-8a9ea94bc833
sprint_title: External agent CLIs as shaped Bash# adapters + AWS Bedrock
closed: 2026-11-07T04:13:31.329732Z
closed_by: claude-opus5.5-s584
---

UnsafeLaunchFlags (pkg/agentlaunch/launch.go:621) matches single tokens, so zcode's '--mode yolo' bypasses the guard and ReadOnly stripping. canonicalUnsafeFlag lacks muse (--yolo) and zcode. Fix: token-sequence matching on the final argv (also --mode=yolo); strip both tokens under ReadOnly. Verify: agentlaunch tests refuse zcode on an uncontained host. Spec: docs/sprint-agent-cli-adapters-plan.md
