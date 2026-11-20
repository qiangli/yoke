---
id: f23921845b5a
kind: doc
title: yoke dag.md build/test descriptions describe coreutils (copied text); now shown in the generated Makefile and make help
seq: 3148
status: todo
priority: p3
created: 2026-11-20T20:02:17.893615Z
---

Found 2026-11-20 reviewing S678 S1: yoke dag.md 'build' says 'Build the busybox-style multicall binary (coreutils <tool>...)' and 'test' says 'Test coreutils' own packages'. Since bashy dag --makefile uses the descriptions as Makefile comments and make help output, fix the text in dag.md and regenerate (bashy dag --makefile).
