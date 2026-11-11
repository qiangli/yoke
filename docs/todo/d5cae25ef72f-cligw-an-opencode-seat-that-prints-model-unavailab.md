---
id: d5cae25ef72f
kind: bug
title: 'cligw: an opencode seat that prints ''Model unavailable'' is served as 200 with empty content'
seq: 3099
status: todo
priority: p2
created: 2026-11-11T13:06:43.753404Z
---

Sprint 613: opencode v2.2.6 has no amazon-bedrock/zai.glm-5 in its catalog ('Error: Model unavailable'), yet the door returned 200 with empty content for model bedrock-glm-5 (routed agent=opencode-bedrock-glm-5, audit status 200, cost 0.0000168). Expected a classified error so routing fails over. Also the seed id itself is stale for current opencode (S584 B1 'not verified live').
