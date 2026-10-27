---
id: 01a17e9d-8df3-7d9c-8b51-d9b8033a15a2
seq: 8
form: page
type: gotcha
title: 'Windows shell paths: program names and file operands use different conversions'
description: WHEN a Bash or Bash# fixture passes paths into Go on Windows, convert executable program paths with pathconv.NativePath (program only, never argv), but convert filesystem operands such as /tmp state files with pathconv.ToOS so virtual mounts resolve too. A launch can succeed yet its evidence file remain missing if these boundaries are conflated.
status: candidate
source:
    tool: codex-gpt5.6-sol-g
    episode: ep-97d96f29b1c8ddc0
    parent_episode: ep-0d5cd81e0fb8b119
created: "2026-10-27T18:26:57Z"
---
