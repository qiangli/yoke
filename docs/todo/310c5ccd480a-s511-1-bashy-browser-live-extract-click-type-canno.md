---
id: 310c5ccd480a
kind: bug
title: 'S511.1 bashy browser live: extract/click/type cannot reach elements inside iframes (incl. cross-origin sandboxed frames)'
seq: 2768
status: done
priority: p0
labels:
    - browser
created: 2026-10-27T00:05:03.280226Z
assignee: s511-claude-manager
sprint: 511
sprint_id: 4a0e4dfe-3f34-5f82-aaad-0656bcdbddc2
sprint_title: 'bashy browser live mode: drive real-world forms (iframes, custom dropdowns, controlled inputs, precise click targets)'
closed: 2026-10-28T19:41:24.388751Z
closed_by: s511-claude-manager
---

Seen 2026-10-26 applying for Microsoft for Startups: portal.azure.com renders its forms (investor-code step, Azure account setup) inside sandbox-*.reactblade.portal.azure.net iframes; extract listed only the outer shell, 'click --text "Continue without a code"' found nothing, so the founder had to fill the whole form by hand. Fix: the MV3 extension enumerates frames (chrome.webNavigation.getAllFrames + scripting.executeScript allFrames/frameIds) and extract/click/type address elements per frame (indexes stay unique; a frame path in output). Red/green: local fixture page with a same-origin and a cross-origin iframe containing a button and an input; live re-check on the Azure Startups investor-code page.
