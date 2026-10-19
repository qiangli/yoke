---
id: c9b3c4896feb
kind: bug
title: 'inbox: bus items (topic conductor.408) never marked read for claude-opus5.5 -- a draining read re-shows the same 12 items each time, so the overflow notice re-injects every turn'
seq: 1900
status: done
priority: p1
created: 2026-10-11T09:30:44.374309Z
assignee: s9999-inbox-cap
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:43:14.051417Z
closed_by: s9999-maintenance-manager
---

Bug: draining unified inbox for claude-opus5.5 re-shows the same 12 bus items on conductor.408, causing overflow reinjection every turn. Determine why exact per-subscriber watermarks/role cursors are not acknowledged; repair source-owned read/ack paths without silently dropping messages. Acceptance: deterministic fixture reproduces repeated bus/role inputs, a drain or sprint inbox-ack consumes only the shown batch, second drain has no repeats, newer unshown input remains unread, peek does not consume. Keep tests of unauthorized readers and cross-source behavior. Scope pkg/inbox/bus and sprint inbox-ack only; avoid weave_impl.go workspace/identity and feed changes. Read CLAUDE.md. Short unit tests only on dev-host, full suites/test builds remote. Correct own BASHY_AGENT/Agent trailer, commit required Sprint #9999 Story #1900 Story-ID c9b3c4896feb; no push, send SHA/tests to s9999-maintenance-manager.
