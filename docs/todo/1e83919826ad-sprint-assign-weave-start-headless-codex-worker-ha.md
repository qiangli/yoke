---
id: 1e83919826ad
kind: bug
title: 'sprint assign/weave start: headless codex worker hangs on inherited stdin (''Reading additional input from stdin'')'
seq: 3113
status: todo
priority: p2
labels:
    - weave
created: 2026-11-13T08:08:59.036819Z
---

S627 2026-11-13: run umbrella#35 (codex, M2b-image) launched by 'bashy sprint assign' from an agent tool shell whose stdin stayed open; codex exec printed 'Reading additional input from stdin...' and waited until max-runtime killed it (80 min, no codex session file, empty result log). Relaunch with '</dev/null' started a session in 2 s. Fix: weave start must give non-PTY workers stdin=/dev/null (or pass the prompt so codex does not read stdin).
