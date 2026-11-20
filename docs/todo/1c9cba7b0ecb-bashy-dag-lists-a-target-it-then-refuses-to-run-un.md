---
id: 1c9cba7b0ecb
kind: bug
title: bashy dag lists a target it then refuses to run ('unknown target') when dag.md has no '## Tasks' section
seq: 3149
status: todo
priority: p2
created: 2026-11-20T20:47:28.20862Z
---

Found 2026-11-20 by the conductor smoke-testing S678 on bashy dc18a4af; same on installed rc.38 (pre-existing). A dag.md with front matter (or not) and '### hello' + bash fence but no '## Tasks' heading: 'bashy dag --list' shows hello; 'bashy dag hello' and 'make hello' (S678 fallthrough) exit 2 'dag: unknown target "hello"'. Adding '## Tasks' fixes it. List and run must use one parser (or both must report the missing section). Root cause first. Matters more now that make forwards to dag and --makefile generates rules from the listed targets.
