---
id: b4e290d640e7
kind: feature
title: 'S529.1 Chrome Web Store listing: publish the live extension so install is one click (no Developer mode / Load unpacked)'
seq: 2825
status: doing
priority: p1
labels:
    - browser
    - release
created: 2026-10-29T01:59:00.723186Z
sprint: 651
sprint_id: 9f9ec0c8-1df1-518f-9843-991ce50f6ee4
sprint_title: 'bashy live extension: store distribution + cross-browser (Chrome Web Store, Edge Add-ons/Windows, Safari, one release target)'
---

Removes the last manual install step (Sprint 511/512 follow-up). Uses the S512.3 'bashy dag chrome-ext-release' zip. Work: (1) operator-held prerequisites via bashy ask/meet — Chrome Web Store developer account under DHNT Inc. (one-time fee), publisher verification; (2) listing: name/description/icons/screenshots, a privacy policy page (loopback-only, no data leaves the machine, no analytics), single-purpose statement; (3) PERMISSION JUSTIFICATIONS — debugger and <all_urls> are critical-risk and get manual review: explain trusted keystrokes / background-tab capture / user-chosen sites, or ship a reduced store variant (optional_permissions + optional_host_permissions requested at runtime) if review refuses; (4) stable extension ID: the hub accepts both the store ID and the unpacked dev ID, version floor still enforced; (5) 'bashy browser setup live' opens the store page when the store build is available, unpacked stays the dev path; (6) store auto-update and the hub hot-reload must not fight (hot-reload only for unpacked). Upload is a human-gated (@confirm) dag step with credentials from bashy secret. Test: release target dry-run zip byte-identical; install from store on the macOS dev box, connect, run the S511 fixtures; hot-reload skipped for store installs.


Update 2026-11-16 (operator): store description gains "Works better with (optional): the bashy tray and outpost", linking both; the required parts are the extension and the bashy command. Hub gate and served sheets: S529.10, S529.7.

Submitted for review 2026-11-16 (operator): item lfbmlehkecabhobacnppikmhgmpnfclm, extension 0.11.1 (zip sha256 b7a80da5...), Unlisted, publisher DHNT Inc (trader verification pending: CP 575 + utility bill). Host-permission justification strengthened against the dashboard advice (activeTab / site list rejected). Open: review outcome; then set live.ChromeWebStoreURL and close.

Scope 2026-11-25 (operator): Sprint 651 supports Chromium only — Chrome and Edge on Windows and macOS. Safari (S529.3/S529.4) and per-store packaging (S529.5) are parked off the sprint. Remaining here: set live.ChromeWebStoreURL to https://chromewebstore.google.com/detail/bashy-live/lfbmlehkecabhobacnppikmhgmpnfclm once Sprint 717 publishes 0.12.0 (store listing checked 2026-11-25: public, Add to Chrome, version 0.11.1, updated 2026-11-18), then a store install connects on Chrome for macOS and Windows (reuse 717's verify evidence). Item (4) needs no code: the hub does not check extension IDs.
