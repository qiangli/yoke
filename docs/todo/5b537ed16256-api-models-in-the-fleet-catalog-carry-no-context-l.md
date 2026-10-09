---
id: 5b537ed16256
kind: bug
title: API models in the fleet catalog carry no context_length, so genie runs glm-5.3 under a 32768-token fallback (20480 usable); and the glm api_key_ref 'zai' does not match the host's bound secret name
seq: 37
status: done
priority: p1
labels:
    - genie
    - llm
created: 2026-10-09T16:39:33.11776Z
assignee: claude-sonnet5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-09T17:43:31.694326Z
closed_by: claude-opus5.5
---

Found 2026-10-09 by the S379 genie fix (ycode d612618 report): yoke pkg/fleet/baseline/models/glm-5.3.yaml has no context_length; bashy internal/agentos/genie_external.go resolveGenieExternal falls back to ctx=32768 when ContextLength<=0, which genie turns into context_budget 20480 (minus output and compaction reserves). GLM-5.x and other API models have far larger windows, so the model is starved and loses tool output early. Fix: add the vendor-documented context_length (and max output if the schema has one) to every API model yaml in the baseline catalog (cite the vendor doc in a comment), and make the fallback loud (warn once) rather than silently tiny. Second: the catalog api_key_ref for glm is 'zai' but on this host the bound secret resolves as 'host-zai'; check how api_key_ref names map to host secret bindings (yoke pkg/secrets, secrets.map) and fix the resolution so a standard name works on every host, with a test.
