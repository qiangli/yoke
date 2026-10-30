---
id: b97f740e230f
kind: bug
title: 'S536.1 follow-up: password-input read exception and write-path coverage'
seq: 2946
status: done
priority: p1
created: 2026-10-30T17:12:41.157074Z
weave: 733
assignee: claude-sonnet5.5
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T17:42:17.494763Z
closed_by: s536-manager
---

Finish S536.1 acceptance gap: suppress raw password-input values before extract/text/evaluate results leave browser context in live/probe/solo; preserve structured results and ordinary visible text. Do not restore generic vault/validator read masking. Direct eval returning password input .value and structured objects containing it must be covered by actual-browser fixtures. This is result sanitization, not a promise to sandbox hostile arbitrary JS. Keep vault matching on writes. Cover explicit field-targeted dispatch/paste writes through guard consistently. Existing guard foundation candidate99d713a302afed5c512bb0861cea0f5ecb553fa2 is reviewed and graded; obtain this dependency into isolated worker branch before changes (manager will supply source location via run steering if needed). Own a small shared read-result sanitizer plus evaluate result hooks and focused fixtures; coordinate cdpactions/guard.go and background selector dispatch with selector worker (run732) before editing overlapping lines. Selector worker owns shared selector resolver; reuse it rather than duplicate. Core tab targeting is separate run731. No edits to unrelated sources or generated KB/WEAVE_MEMORY artifacts. Short units only on development host; manager runs actual Chrome remotely. Commit with supplied provenance and report exact tests/remaining limitations; no push/merge to main.
