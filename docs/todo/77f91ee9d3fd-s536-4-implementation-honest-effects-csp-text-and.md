---
id: 77f91ee9d3fd
kind: feature
title: 'S536.4 implementation: honest effects, CSP text and inner-panel capture'
seq: 2948
status: done
priority: p2
created: 2026-10-30T17:17:21.125339Z
weave: 735
assignee: muse-conductor
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T18:35:49.913054Z
closed_by: s536-manager
---

Implement S536.4 on the concrete isolated integration candidate04837256: fetch origin/s536-integration-candidate and fast-forward your worker branch before implementation. Guard and tab outcomes are accepted; selector candidate is assembled but remote gate pending while Sprint537 uses test host. This independent effects lane can implement against that API; final integration still waits for every P1 gate. Click reports observed navigated/dom-changed/dialog/no-effect with bounded observation; browser text [selector] reads visible text via isolated-world DOM under strict CSP without eval, preserving password-input suppression. Scroll nearest scrollable ancestor of addressed element. screenshot --full-page stitches inner scroll region and restores page/panel positions even on failure. Reuse shared tab/selector resolution, keep live/probe/solo coherent, update CLI/wire/help/atlas as needed. Own click/text/scroll/capture implementations and focused tests; live selector followup2a07eacd0aaf owns background dispatchEventInTab, waitForSelector, keyboardPress and their page helpers. Do not modify those or guard policy/sanitizer. No new desk/overlay scope. Use existing real Chrome runners for inert/effectful click, strict CSP, inner-panel scroll/capture (verify image extent/content and restored position); provide red/green fixtures and exact gate commands. Short units only on development host; manager owns remote Chrome/build. Commit with trailers; no push/merge, no generated KB/WEAVE_MEMORY changes. Communicate through bashy mb send --as your seat s536-manager.
