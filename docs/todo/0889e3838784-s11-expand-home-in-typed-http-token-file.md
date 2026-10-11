---
id: 0889e3838784
kind: bug
title: S11 expand home in typed HTTP token_file
seq: 1900
status: done
priority: p1
labels:
    - yoke
    - toolcmd
created: 2026-10-11T09:38:01.691186Z
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T09:52:36.557716Z
closed_by: codex-gpt6-sol
---

Live Codync host gate on claimed Linux: adapter token_file ~/.codync/token is passed literally to os.ReadFile, causing no such file despite token at owner home. Add generic, bounded home expansion for token_file only: leading ~/ resolves through os.UserHomeDir, no shell/env eval and no arbitrary variable expansion. Preserve security/refusal behavior and test exact expansion plus missing-file refusal. The same declaration then works on every host; no Codync-name conditional. Manager-owned direct fix under original S11 HTTP transport prerequisite and current Sprint418 lease. Run focused pkg/toolcmd tests, claimed Linux live read commands, then pin Yoke into Bashy before accepting original S11.
