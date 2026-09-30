---
id: 6a7ef9086309
kind: bug
title: genie retries a non-retryable 409 sticky refusal with exponential backoff for its whole budget
seq: 18
status: todo
priority: p1
labels:
    - genie
created: 2026-09-30T11:24:03.811104Z
sprint: 332
sprint_id: 5591ec3f-fa56-5047-a968-cd01097cee2a
sprint_title: 'bashy small improvements and bug fixes — #314 follow-up'
---

Steward genie trial 2026-09-30: with the door refusing (409 bashy_broker sticky 'the binding cannot be served exactly'), genie logged 'API error 409 [Unknown -> Retry]' and retried 9 times per turn with backoff up to 3m14s, twice, until weave killed the run at its 12-minute cap (runs #127, #128: 0 output, whole budget burned; the coach then injected 'burned your budget without converging'). A 4xx refusal from the broker is final. Fix (KISS): classify 4xx (except 408/429) as non-retryable in the provider retry policy; fail the turn at once with the error. Red/green: a stub endpoint returning 409 fails in one attempt; 429 still retries.
