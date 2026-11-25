---
id: 52bf65e95658
kind: bug
title: 'shape verify: Markdown @effects atoms can raise a shapeware app''s base cap'
seq: 3156
status: todo
priority: p1
labels:
    - shaping
    - security
created: 2026-11-25T16:05:09.934438Z
sprint: 721
sprint_id: 69fc2c51-8faf-58e7-83ac-f27007837ae7
sprint_title: 'Shapeware pilot on form: spec v0 layout, one-step launch'
---

From the umbrella Sprint 712 vendor review (GLM #2, operator-accepted g1; spec v0.1 §7, finding F7).
`pkg/shape/verify/contract.go` DecoratorReader unions the `@effects`/`@guard` atoms found in .bsh, .bash AND .md (fenced) files. For a shapeware app, Markdown (SKILL.md, references/, known issues) is the easiest injection target, so text could raise the base cap.
Root cause first: confirm which Markdown fences contribute today and why .md was included (dag or skill fences may rely on it), before any change.
Proposed change: for `type: shapeware` folders, take the cap from contract.bsh and code only; ignore Markdown atoms. Other folders unchanged.
Acceptance: a unit test where a SKILL.md fence declares @effects("net") and contract.bsh declares @effects("read"): the base cap is read only; existing verify tests pass.
