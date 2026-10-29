---
id: b4e290d640e7
kind: feature
title: 'S529.1 Chrome Web Store listing: publish the live extension so install is one click (no Developer mode / Load unpacked)'
seq: 2825
status: todo
priority: p1
labels:
    - browser
    - release
created: 2026-10-29T01:59:00.723186Z
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
---

Removes the last manual install step (Sprint 511/512 follow-up). Uses the S512.3 'bashy dag chrome-ext-release' zip. Work: (1) operator-held prerequisites via bashy ask/meet — Chrome Web Store developer account under DHNT Inc. (one-time fee), publisher verification; (2) listing: name/description/icons/screenshots, a privacy policy page (loopback-only, no data leaves the machine, no analytics), single-purpose statement; (3) PERMISSION JUSTIFICATIONS — debugger and <all_urls> are critical-risk and get manual review: explain trusted keystrokes / background-tab capture / user-chosen sites, or ship a reduced store variant (optional_permissions + optional_host_permissions requested at runtime) if review refuses; (4) stable extension ID: the hub accepts both the store ID and the unpacked dev ID, version floor still enforced; (5) 'bashy browser setup live' opens the store page when the store build is available, unpacked stays the dev path; (6) store auto-update and the hub hot-reload must not fight (hot-reload only for unpacked). Upload is a human-gated (@confirm) dag step with credentials from bashy secret. Test: release target dry-run zip byte-identical; install from store on Dragon, connect, run the S511 fixtures; hot-reload skipped for store installs.
