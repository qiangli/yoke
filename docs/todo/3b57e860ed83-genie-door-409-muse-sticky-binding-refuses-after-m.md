---
id: 3b57e860ed83
kind: bug
title: 'genie door 409: muse sticky binding refuses after Muse Code CLI upgrade (1.3.0 -> 1.4.3)'
seq: 1982
status: done
priority: p1
labels:
    - genie
    - door
created: 2026-10-12T10:08:24.551097Z
weave: 187
assignee: s9999-genie-rework
sprint: 9999
sprint_id: 559b491b-4bab-57d3-bad4-5ceb9acbb9da
sprint_title: Maintenance
closed: 2026-10-12T15:44:50.486127Z
closed_by: s9999-maintenance-manager
---

Observed by s426-manager 2026-10-12 ~10:08-10:35Z. ALL genie door routes are broken on Dragon: (1) bashy genie -m door-muse-spark1.3 -> 409 'sticky: identity 15c72b581cd4: muse is now Muse Code 1.4.3 (was 1.3.0); the binding cannot be served exactly'. (2) door-agy-gemini3.8-flash -> 409 'identity 40fa7d356d1f: agy is now 1.3.3 (was 1.2.13)'. genie's per-model stickies (genie-<model>, principal-owned, ttl 0) are frozen to the agent CLI version, and the CLIs have moved. (3) Recovery attempt: s426 deleted genie-gemini3.8-flash and re-created it with bashy llm sticky create ... --ttl 0 (and again with --export): genie then gets 404 'sticky key is owned by session s-..., not this one' because genie runs its turn in a nested dag/ycode door session; the principal-owned form could not be recreated from a shell. s426 removed its session sticky, so genie-gemini3.8-flash is now ABSENT (genie gets 404 no binding). genie-muse-spark1.3 is untouched (still 409). Also: genie -m muse-spark1.3 (no door- prefix) tries an ollama pull and fails 'pull model manifest: file does not exist'. Needed: a supported way to re-pin a genie sticky after a CLI upgrade (or genie creating its sticky at principal scope when absent), plus a check that agent CLIs did not self-update.

S9999 lane: Genie/sticky/door only. Preserve exact-version guarantees; supported principal-owned re-pin/recovery after CLI upgrade with stale/missing ownership tests. No metered fallback or silent model switch. No push, no canonical changes, no containers/full builds on Dragon. Send SHA/tests and live recovery remainder to s9999-maintenance-manager.
