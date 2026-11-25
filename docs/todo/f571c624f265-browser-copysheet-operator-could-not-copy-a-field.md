---
id: f571c624f265
kind: bug
title: 'browser copysheet: operator could not copy a field a second time'
seq: 3154
status: todo
priority: p3
labels:
    - browser
created: 2026-11-25T10:17:41.648066Z
---

Symptom (2026-11-25, operator, Chrome split view, live hub): after copying a field once on the sheet beside github.com/account/organizations/new, the operator could not copy it again and needed the values pasted in chat.

Code inspected (cmds/browser/copysheet.go): a copied field only gets .done (opacity .5), and the button resets to 'Copy' after 1.5 s. Nothing in the page disables a second copy. Root cause NOT identified. Candidates: navigator.clipboard.writeText refused on the second click (focus in the other split pane, or a missing user-activation), or the greyed state reading as disabled.

Next: reproduce in the live hub split view, capture the console error on the second click, then classify (product vs browser policy). Expected behaviour: a field can be copied any number of times.
