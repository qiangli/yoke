---
id: "940999280048"
kind: bug
title: 'S511.4 bashy browser live: click --text picks the wrong element (nav links, headings, dialog close)'
seq: 2771
status: done
priority: p0
labels:
    - browser
created: 2026-10-27T00:05:10.245352Z
assignee: s511-claude-manager
sprint: 511
sprint_id: 4a0e4dfe-3f34-5f82-aaad-0656bcdbddc2
sprint_title: 'bashy browser live mode: drive real-world forms (iframes, custom dropdowns, controlled inputs, precise click targets)'
closed: 2026-10-28T19:40:34.93803Z
closed_by: s511-claude-manager
---

Seen 2026-10-26: 'click --text Industry' clicked the site menu link 'Industry solutions' on cloud.google.com and left the form; 'click --text "Create budget"' hit the page heading in the AWS console, not the submit button; on AWS Activate an unlabeled <button> picked by index was the dialog close (X) and discarded the form. Fix: rank matches by exact text > prefix, interactive role (button/input/option) over link/heading, visible and inside the topmost dialog/form first; refuse ambiguous matches with candidates listed; extract labels icon-only buttons by aria-label/title/svg (e.g. 'Close'). Red/green: fixture with nav link + heading + button sharing text and a modal with an icon close; live re-check on the AWS Budgets create page.
