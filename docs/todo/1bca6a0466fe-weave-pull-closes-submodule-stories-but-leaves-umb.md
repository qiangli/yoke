---
id: 1bca6a0466fe
kind: bug
title: weave pull closes submodule stories but leaves umbrella-root stories 'assigned'; assign --repo SUB cannot find umbrella stories and goal link cannot link submodule stories
seq: 2037
status: todo
priority: p3
created: 2026-10-23T17:35:05.291872Z
---

Found 2026-10-23 Sprint 458. (1) weave pull of yoke/bashy runs set the story to done, but umbrella run #131 (M3b) left 9154f8ae 'assigned' until the manager linked and accepted it. (2) sprint assign 458 b9b3580c --repo yoke: 'story not found in any root tracked by sprint'; the manager had to refile stories in each submodule. (3) sprint goal link 458 m1 --story <yoke story>: 'no issue in the register'. Expected: one consistent closure path and cross-root story lookup for the umbrella's tracked roots.
