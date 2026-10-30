---
id: 2a07eacd0aaf
kind: bug
title: 'S536.3 follow-up: live selector parity for keyboard, waits and dispatched events'
seq: 2950
status: done
priority: p1
created: 2026-10-30T17:41:58.633383Z
weave: 734
assignee: claude-sonnet5.5
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T18:01:15.235909Z
closed_by: s536-manager
---

Finish explicit remaining selector acceptance: background.js dispatchEventInTab, waitForSelector and keyboardPress still use document.querySelector. Route CSS/XPath/text=/label= through shared page resolver with open-shadow and same-origin frame targeting. Missing/hidden/ambiguous keyboard target must fail before any trusted key event, never silently type into another focused element. Keep dispatch event listener behavior and frame routing correct. Add actual extension Service end-to-end fixtures for XPath/label wait, dispatch, keyboard and missing-target no mutation. Base dependency: fetch origin/s536-selector-reviewed and fast-forward isolated branch (source branch contains latest selector foundation591122de). Guard/privacy lane is separate and merges later: do not modify guard policy, evaluate sanitizer or CDP guard adapter. Own live background dispatch/wait/keyboard functions and corresponding page helpers/tests only. No click effects/scroll/screenshot/text action work (separate S536.4). Short units only on development host; manager owns remote Chrome. Commit with trailers; no push/merge or KB/WEAVE_MEMORY edits. Communicate with bashy mb send --as your seat s536-manager and report exact fixtures.
