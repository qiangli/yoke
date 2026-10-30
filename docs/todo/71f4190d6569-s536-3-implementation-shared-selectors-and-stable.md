---
id: 71f4190d6569
kind: feature
title: 'S536.3 implementation: shared selectors and stable field references'
seq: 2945
status: done
priority: p1
created: 2026-10-30T17:03:38.915387Z
weave: 732
assignee: codex-gpt6-sol
sprint: 536
sprint_id: b0c9a7d1-5771-5621-bba5-6c2ed640673f
sprint_title: 'bashy browser hardening: read-only never blocked, tab targeting, XPath/CSS selectors, honest effects'
closed: 2026-10-30T18:01:16.2604Z
closed_by: s536-manager
---

Implement S536.3: CSS, XPath (xpath= or leading / or (), text= and label= for browser reads and writes; traverse open shadow roots and same-origin iframes. Extract prints stable selector beside index and structured field metadata for consumers: stable id, label, selector, tag/name; retain existing result fields. Index writes must check extract-time identity (tag+name+label) and refuse shifted/different targets with guard:fill-mismatch style error, not silently fill another field. Guard checks and writes must resolve the SAME element. Existing frames enumeration must remain coherent. Inspect live extension/page.js, frames.js and CDP action paths; share selector resolver rather than incompatible mode implementations. Own new selector helpers + selector/extract/index portions of page.js and focused fixtures. Concurrent guard lane run730 owns guard.js, guard.go and read-masking/guard sections in page.js; tabs lane run731 owns browser.go, live/service.go and background tab dispatch. Do not edit their portions without coordination. Prepare selector code independently, report interface needs to s536-manager; integration waits on foundations. Add focused red/green tests dynamic insertion/re-render, same-field value changes, XPath/text/label, shadow/frame targets. Sprint 537 replyform needs stable field id/label/selector and refusal reason; agree additive JSON shape with manager before final commit. the development host only short units, no browser/full build; manager runs remote tests, send fixture names. Commit with trailers, no push/merge. No proprietary umbrella content/real hostnames/user details in OSS docs.
