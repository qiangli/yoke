---
id: b18d96e68f04
kind: refactor
title: 'dag: silence spurious effect-cap warning via real-target/exec self-classification'
seq: 30
status: todo
created: 2026-10-06T04:30:14.101213Z
sprint: 382
---

Sprint 343 genie-investigation follow-up. classifyCommand mistook a dag flag for the target (bashy dag -f f.md T -> 'dag -f' unknown) and marked self script/-c invocations unknown. Fix: resolve the real last-arg dag target when in this dag, else classify self-recursion as exec (the shell dispatches a child it no longer governs; atlas does not classify the shell). Advisory task-cap warning now fires only for genuinely undeclared effects.
