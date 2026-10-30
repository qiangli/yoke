---
id: e5da5e3544d5
kind: bug
title: 'S536 live acceptance: extract and label-target contenteditable plaintext-only form fields'
seq: 2952
status: done
priority: p1
created: 2026-10-30T22:24:33.189883Z
weave: 738
assignee: claude-sonnet5.5
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T22:46:02.087483Z
closed_by: s536-manager
---

Live Airtable recreated test form revealed remaining defect: contenteditable=plaintext-only long-text field omitted by extraction and label selector says no match, while type CSS [contenteditable] succeeds and text reads exact value. page.js CANDIDATES omits editable controls; isEditable only empty/true. Fix shared extraction/selector and relevant guard/CDP paths for editable true/empty/plaintext-only while excluding false. Preserve field label/stable metadata; test actual aria-labelledby or label markup. Red/green fixtures, browser regression, live same form recheck without submit. Keep public commits free of private account URLs. Sprint536 integration2947 depends on this.
