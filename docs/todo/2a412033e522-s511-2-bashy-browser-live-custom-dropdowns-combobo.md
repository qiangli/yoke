---
id: 2a412033e522
kind: bug
title: 'S511.2 bashy browser live: custom dropdowns/comboboxes/listboxes are not listed or selectable'
seq: 2769
status: done
priority: p0
labels:
    - browser
created: 2026-10-27T00:05:05.654451Z
assignee: s511-claude-manager
sprint: 511
sprint_id: 4a0e4dfe-3f34-5f82-aaad-0656bcdbddc2
sprint_title: 'bashy browser live mode: drive real-world forms (iframes, custom dropdowns, controlled inputs, precise click targets)'
closed: 2026-10-28T19:41:25.413478Z
closed_by: s511-claude-manager
---

Seen 2026-10-26: Google for Startups form 'Industry' and 'Job role' (Material select, no <select>), AWS Activate profile 'State / Province' (custom listbox after Country change), Azure signup radio groups rendered hidden. extract showed nothing usable; clicking the label text navigated away instead. Fix: extract lists ARIA widgets (role=combobox/listbox/option/radio, aria-haspopup, aria-expanded) with their current value, and a select-style action (open + pick option by visible text, typing to filter when a search box appears). Red/green: fixture with Material-style select, ARIA listbox with search, and hidden-input radio group; live re-check on the Google for Startups Industry field.
