---
id: 944b0ce306d6
kind: bug
title: 'S511.3 bashy browser live: type reports ''typed'' but React/controlled inputs discard the value'
seq: 2770
status: todo
priority: p0
labels:
    - browser
created: 2026-10-27T00:05:07.954437Z
sprint: 511
sprint_id: 4a0e4dfe-3f34-5f82-aaad-0656bcdbddc2
sprint_title: 'bashy browser live mode: drive real-world forms (iframes, custom dropdowns, controlled inputs, precise click targets)'
---

Seen 2026-10-26: Azure signup 'Organization name'/'Job title' and the Google for Startups form (after history back) showed empty fields after 'typed'; AWS Activate needed 'type --index' because input:focus never matched after click --index. Fix: type uses the native value setter + dispatches beforeinput/input/change (and key events where needed) so React/Angular/Lit state updates; verify by reading the value back and report a mismatch instead of 'typed'; click --index leaves the element focused. Red/green: fixture with React controlled input and an Angular-style form; live re-check on the Azure org-name step.
