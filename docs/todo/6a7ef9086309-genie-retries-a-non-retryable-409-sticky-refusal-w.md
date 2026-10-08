---
id: 6a7ef9086309
kind: bug
title: genie retries a non-retryable 409 sticky refusal with exponential backoff for its whole budget
seq: 18
status: done
priority: p1
labels:
    - genie
created: 2026-09-30T11:24:03.811104Z
assignee: claude-opus5.5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-08T19:16:16.416832Z
closed_by: claude-opus5.5
---

Steward genie trial 2026-09-30: with the door refusing (409 bashy_broker sticky 'the binding cannot be served exactly'), genie logged 'API error 409 [Unknown -> Retry]' and retried 9 times per turn with backoff up to 3m14s, twice, until weave killed the run at its 12-minute cap (runs #127, #128: 0 output, whole budget burned; the coach then injected 'burned your budget without converging'). A 4xx refusal from the broker is final. Fix (KISS): classify 4xx (except 408/429) as non-retryable in the provider retry policy; fail the turn at once with the error. Red/green: a stub endpoint returning 409 fails in one attempt; 429 still retries.
