---
id: 1dcb7bedf966
kind: bug
title: 'fleet seeds: Bedrock API models need ids.ycode = the fleet model name (genie resolves external models by name)'
seq: 3098
status: todo
priority: p1
created: 2026-11-11T13:06:43.687422Z
---

Sprint 613 live check (dev box door, Bedrock us-east-1): seat ycode-bedrock-glm-5 launches 'bashy genie -m zai.glm-5' because bedrock-glm-5.yaml has no ids.ycode and {model} falls back to the provider id; genie only resolves an external API model by its fleet name, so it falls through to local ollama and exits 1 (cligw 502 transient). With 'bashy model set bedrock-glm-5 --set ids.ycode=bedrock-glm-5' (host overlay) genie answers through Bedrock. Same gap for bedrock-gpt6-sol and bedrock-opus4.8 (zero-ycode seats). Fix: add ycode/zero-ycode ids to the three seeds; test: agent verify prints genie -m <fleet name>.
