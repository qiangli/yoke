---
id: 88d62fd745c7
kind: enhancement
title: Layered telemetry retention and useful agent-memory rollups after emergency storage repair
seq: 3048
status: todo
priority: p2
labels:
    - telemetry
    - follow-up
created: 2026-11-05T14:41:00.530085Z
---

Follow-up to P0 e385262ac250. Preserve its comprehensive design requirements: relevance-oriented deterministic filtering/dedup/state folding; protected agent-lifetime originals; daily rotation and automatic weekly/monthly/yearly rollups; evidence-linked memory candidates via existing KB seams; versioned extensible policy/schema, replay/late-arrival/calendar correctness, safe publication/expiry and independent bounded scratch. Reuse reviewed rollup helper commits and in-progress storage design; retain all existing work. This feature MUST NOT delay the minimal producer polling-noise plus rotation/disk-bound repair or resumption of Bash# tests/examples. Implementation scheduled after P0 is safely deployed; no expanded platform rewrite.
