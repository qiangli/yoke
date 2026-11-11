---
id: 777074a1c6ae
kind: feature
title: 'door: serve fleet API models (Bedrock) as a plain OpenAI-compatible passthrough so tool-calling clients (opencode) work'
seq: 3100
status: done
priority: p1
created: 2026-11-11T17:16:55.516982Z
closed: 2026-11-11T17:52:54.148954Z
---

Sprint 613 live: through the lern-si satellite a member's opencode works on the door's local model (gpt-oss:20b, GPU) but fails on bedrock-glm-5: the door reaches Bedrock only through agent-CLI seats (cligw ycode/zero), which return agent transcripts (one reply was an unrelated 'created a calculator module' summary, another 'door-okDONE') and go transient under opencode's tool-bearing requests ('no matching candidate is installed and outside breaker cooldown'). broker.Options carries only Engine and CLI. Wanted: api-kind fleet models (base_url + api_key_ref, e.g. bedrock-glm-5 with the host-minted key) served as a direct passthrough, metered and peer-gated like the rest of the door.
