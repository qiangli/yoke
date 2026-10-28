---
id: e1cbc5679f0b
kind: bug
title: 'S512.6b probe/solo: first-visible-match selector resolution + bounded timeouts (the S512.6 Typeform selector hang)'
seq: 2824
status: done
priority: p0
created: 2026-10-28T23:21:05.146602Z
weave: 665
assignee: muse-conductor
sprint: 512
sprint_id: 9f31a532-c25d-5504-8bd8-163f39bd871d
sprint_title: 'bashy browser site skills: the Bash# skill/script duality applied to web pages (thin extension core + per-site skills, self-healing)'
closed: 2026-10-28T23:29:25.827579Z
resolution: fixed
closed_by: muse-conductor-o
---

Part of S512.6 (02e95627732f, umbrella register): the core bug that story found, filed in the yoke register only because a yoke run needs a yoke story. Not new scope.
== NEXT RUN (yoke): the core selector hang you found (manager-diagnosed 2026-10-28) ==
Your run 199 delivery (skills/typeform-fill 03b9e7c5f) is preserved at umbrella refs/s512/run-199 and will be carried forward in a later umbrella run; THIS run is repo yoke only.
CAUSE: probe/solo (pkg/browser/internal/cdpactions/actions.go) resolve selectors with chromedp ByQuery = the FIRST match in document order. On Typeform-style pages that first match of `input, textarea` / `input:not(...)` is usually inside a hidden question, so WaitVisible waits for that hidden node until timeout, and SendKeys (type), Click and Focus wait for it to become visible with NO timeout -> hang. Live mode already prefers the first VISIBLE match (S511.3: type/select refuse hidden-only, --include-hidden overrides).
FIX: give probe/solo the same semantics: resolve the first visible match of the selector (comma lists and :not() must work), refuse hidden-only with the same message as live unless --include-hidden, and bound every chromedp action with the action's timeout (default 5s), so nothing can hang. Keep the S512.5 guard path intact (it runs before these actions).
Tests: a solo/headless fixture with a hidden first match + a visible later match for `input, textarea` and `input:not([type=hidden])`: type/click/wait-for-selector act on the visible one; hidden-only refused; nothing exceeds the timeout.
Rules: Dragon = focused tests (go test ./pkg/browser/... ./cmds/browser, BASHY_BROWSER_LIVE=1 for the headless page tests). Commit on your branch, do NOT push. Trailers: this story's Sprint/Story/Story-ID (bashy todo show).
