---
id: ca7ca1aa4e35
kind: feature
title: 'A4 per-peer limits: bashy llm peer limit NAME --tokens-day --usd-month --rpm'
seq: 3071
status: done
priority: p1
created: 2026-11-10T01:04:30.479604Z
weave: 24
assignee: claude-opus5.5
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
closed: 2026-11-10T02:22:55.185817Z
closed_by: claude-opus5.5-s602
---

Store limits as fields on PeerShare in peers.json. Enforce in Broker.ServeHTTP right after authorizePeer/peer-route check (the gateway FilterCandidatesFunc cannot see the peer: delegateCLI swaps the token). In-memory counters seeded from the A3 rollup at start; RPM sliding window; over-limit returns 429 with a JSON reason and Retry-After/reset (queue-full pattern ~broker.go:674). Owner unlimited; one in-flight overshoot accepted. No llmbudget reservations. Tests: unit + the sprint integration httptest: two peers, fake backend with fixed usage, daily cap; peer 1 gets 429 with reason+reset, peer 2 still 200, llm usage --json totals match. Testing (operator rule): unit tests plus at most one minimal integration test in this story; full suites only at sprint close or release.
Manager note (A1 c0ff5c74 + A3 fb9c7337 merged): seed counters with broker.RollupUsage (pkg/broker/usage.go). RollupUsage fails on any malformed audit line: at door start, log the error and start with empty counters - never block the door from serving. Commit trailers one per line, no blank lines between.
