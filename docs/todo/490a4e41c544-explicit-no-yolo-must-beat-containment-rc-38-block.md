---
id: 490a4e41c544
kind: bug
title: 'explicit --no-yolo must beat containment (rc.38 blocker); precedence: system < shell env/config < command env/config < flag'
seq: 3146
status: todo
priority: p1
created: 2026-11-20T17:32:06.011745Z
---

rc.38 gate in the official image: bashy agy --no-yolo still launched with --dangerously-skip-permissions because FinalizeArgs treats a container as acceptance and --no-yolo only omitted --yolo. Operator decision 2026-11-20: an explicit --no-yolo always wins; general rule for every override: system/env/config defaults < shell env/config < command env/config < command-line flag (most specific wins).
