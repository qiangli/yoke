---
id: ac5981a3a6a3
kind: enhancement
title: 'bashy git checkout (CLI): accept -q/--quiet like the native exec path'
seq: 3107
status: todo
priority: p3
created: 2026-11-12T18:59:15.365179Z
sprint: 621
sprint_id: 12e0ca54-4941-5b64-bc6c-4eb80ac71a59
sprint_title: 'Software Shaping TDD workflow: Red-Green-Refactor verification across web, CLI, and service shaping'
---

S619: run #67 added -q/--quiet to nativeCheckout (exec path), but the bashy git checkout cobra command (git.Checkout, git.go) still rejects -q ('unknown shorthand flag'). Add the flag to the CLI so scripts written for git run unchanged.
