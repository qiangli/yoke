---
id: 438674811fbb
kind: bug
title: 'S536.2 implementation: address intended browser tab for every action'
seq: 2944
status: done
priority: p1
created: 2026-10-30T16:56:15.359801Z
weave: 731
assignee: s536-manager
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T17:32:41.191769Z
closed_by: s536-manager
---

Implement browser tab targeting for umbrella S536.2 fc02326ec872. Every action must honor --url/--title/--id; no match or ambiguity must fail rather than act on active tab. navigate with no address creates a new tab. Include guard preflight/followup target consistency. Own cmds/browser/browser.go, pkg/browser/wire and live/service.go plus extension/background.js tab resolution. Guard lane owns live/guard.go and extension/page.js/guard.js; coordinate interface needs with s536-manager rather than overlapping. Add focused red/green tests through existing CLI/service/extension runners; cover addressed read/write/screenshot/eval/scroll/navigation and unmatched/ambiguous tabs. Read local CLAUDE.md. Commit with supplied trailers, no push/merge. the development host supports short unit tests only; no full builds/browser integration. Send exact remote test commands and progress to manager. Never include private umbrella source, real hostnames or user data in OSS docs.
