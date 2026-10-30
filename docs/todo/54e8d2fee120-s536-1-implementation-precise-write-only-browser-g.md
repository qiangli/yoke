---
id: 54e8d2fee120
kind: bug
title: 'S536.1 implementation: precise write-only browser guard'
seq: 2943
status: done
priority: p1
created: 2026-10-30T16:56:15.244573Z
weave: 730
assignee: s536-manager
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T17:42:18.464224Z
closed_by: s536-manager
---

Implement browser guard fixes for umbrella S536.1 b2ff2daa1ee9. Ordinary AWS ID, company email, Org ID and NVIDIA technology question are unguarded; password, OTP, bank account number, card number and CVV remain guarded. Classify using field-own identity, not describedby/help/surrounding question; account/card need qualifiers. Reads unmasked except password-input values. Preserve vault value matching. Clear human-focus outline on Done/Skip/Stop. Add focused red/green regression fixtures using existing browser tests. Own pkg/browser/guard, guardask, live/guard.go, extension/guard.js and guard/read portions of page.js; avoid CLI/tab dispatch/background.js owned by tab lane unless coordinate first. All browser modes should remain coherent. Operator explicitly authorized this sprint and these guard changes. Read local CLAUDE.md. Commit with supplied trailers, no push/merge, no broad build or browser integration on the development host. Short unit tests only; report exact remote commands needed. Send manager s536-manager concrete progress and blockers promptly. Preserve existing refusal JSON; Sprint 537 consumes it. Never include private umbrella source, real hostnames or user data in OSS docs.
