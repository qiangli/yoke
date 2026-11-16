---
id: f1cd947eb091
kind: bug
title: 'browser: solo/probe navigate hangs 30s on every real page (Text body BySearch)'
seq: 3114
status: done
priority: p1
labels:
    - browser
created: 2026-11-16T10:32:50.504571Z
closed: 2026-11-16T10:36:32.050566Z
---

Symptom: bashy browser --mode solo|probe navigate <any remote URL> -> {success:false,error:"context deadline exceeded"} after 30s; about:blank, data: and a local page with no 'body' text succeed in ~0.4s.

Root cause (product defect, recorded 2026-11-16): cdpactions.Navigate reads the page with chromedp.Text("body", &body, chromedp.NodeVisible) and no query option, so chromedp uses its default BySearch (DOM.performSearch). That matches plain text as well as CSS, so 'body' also hits a non-element node (example.com: the inline CSS body{...}). DOM.getBoxModel on it errors 'Could not compute box model', and NodeVisible waits for EVERY match, so the action retries until the 30s deadline. CDP trace: getSearchResults -> nodeIds [10,17]; getBoxModel(17) fails each poll.

Ruled out: network (curl + raw headless Chrome load the page in 0.5s), Chrome launch (full lifecycle reaches networkIdle), rest of the stack (plain chromedp v0.15.1 hangs only with that exact Text step; adding chromedp.ByQuery fixes example.com/github.com/wikipedia). Line unchanged since 2854c03c (Sprint 208); the trigger is outside the code (likely the Chrome 157 update or page markup), not bisected.

Fix: chromedp.ByQuery on that call + a regression test with a page whose CSS contains 'body'.
