---
id: 4a4f46051be2
kind: bug
title: 'S536.4 follow-up: full navigation and native dialog effects; visible text and shadow scrolling'
seq: 2951
status: done
priority: p2
created: 2026-10-30T18:11:09.85975Z
weave: 737
assignee: claude-sonnet5.5
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T18:46:38.455101Z
closed_by: s536-manager
---

Independent review of effects2065a03f found remaining concrete acceptance gaps. Fetch/merge origin/s536-effects-reviewed (local tracking ref may already exist; source main has one independent S537 doc commit). Own click observation in live background and CDP actions + page click, text visibleTextOf, and scrollTargetScroller, with focused actual-browser tests. Real navigation including same-URL reload must report navigated; native JS dialogs must report dialog with bounded completion and without automatically answering the operator dialog (tests may dismiss their own fixture dialog). Current tests only hash navigation/DOM dialog. Empty innerText must NOT fall back to hidden-only textContent; visible text must exclude hidden roots/subtrees. Scroll ancestor traversal must cross ShadowRoot.host. Keep effects metadata shape and source0.10.0. No screenshot/stitch code edits: run735 Muse owns shotLayout/shotSet/shotRestore, fullpage.go and live capture functions for cleanup/error fixes. No guard policy, stable-ID helpers, or keyboard/wait/dispatch changes. Independent focused fixtures using existing real Chrome runners, no string-only implementation mirror tests. Absolutely no Chrome/BASHY_BROWSER_LIVE=1 or full builds on development host; manager runs remote checks. Short units only. Commit provenance, report exact fixtures and limitations, no push/merge. Communicate with bashy mb send s536-manager as your own seat, NEVER --as s536-manager.
