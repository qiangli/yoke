---
id: 3f136668f641
kind: bug
title: 'sprint assign path launches workers with no idle-timeout: an agy run sat 20 min at 0% CPU with no output until manually abandoned'
seq: 2036
status: todo
priority: p2
created: 2026-10-23T17:35:03.120887Z
---

Found 2026-10-23 Sprint 458 run dhnt#130 (agy gemini3.8-flash, --print-timeout 40m): no PTY capture, no file changes and 0% CPU for 20 min; weave list said 'working'. weave start has --idle-timeout but it defaults off and sprint assign does not set it. Expected: assign-path runs get a default output-idle timeout (e.g. 10m) and report 'stalled' instead of 'working'.
