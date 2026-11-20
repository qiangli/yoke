---
id: 076b8f28ad1a
kind: test
title: 'browser/probe: TestLiveProbeSkippedWhenUnavailable fails when a debug Chrome has no window'
seq: 3142
status: todo
priority: p3
created: 2026-11-20T15:39:44.133699Z
---

2026-11-20 dev box: a Chrome 157 with --remote-debugging-port=9222 answered /json/version but had no window, so the probe attach failed with 'Failed to open new tab - no browser is open (-32000)' instead of skipping. Fails identically on yoke main (not caused by run #95). Venue-dependent test edge: the test skips only when nothing listens on 9222. Root cause to record: decide whether 'no browser is open' means unavailable (skip) or the probe should open a window.
