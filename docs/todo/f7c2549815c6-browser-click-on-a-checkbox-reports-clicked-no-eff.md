---
id: f7c2549815c6
kind: bug
title: 'browser click on a checkbox reports clicked: no-effect although the box toggles'
seq: 3117
status: todo
priority: p2
labels:
    - browser
created: 2026-11-16T14:11:20.839961Z
---

Seen 2026-11-16 (S529.1 screenshot run, live mode, extension 0.11.1): bashy browser click '#terms' on <input type=checkbox> printed 'clicked: no-effect' while document.querySelector('#terms').checked was true afterwards. The effect classifier likely ignores property-only state changes (checked/value) that mutate no attribute. Root cause not yet identified; reproduce in probe/solo too before fixing.
