---
id: f72fdf8525bb
kind: bug
title: 'llm door: the Anthropic endpoint returns 404 for local models (no Anthropic-to-Ollama translation on the local engine path)'
seq: 35
status: done
priority: p0
labels:
    - llm
created: 2026-10-09T14:51:44.168953Z
assignee: claude-sonnet5.5
sprint: 379
sprint_id: 908c2ac2-e7bc-55fe-86bd-d25046ac4684
sprint_title: 'bashy 1.0.0 feature list: bash + Bash# + Yoke'
closed: 2026-10-09T15:22:19.323221Z
closed_by: claude-opus5.5
---

Found 2026-10-09 by the S379 Y3 per-band run on the macOS test host (yoke run 7, qwen3:8b through the bashy llm door): OpenAI /v1/chat/completions 200 and Ollama /api/chat 200, but POST ANTHROPIC_BASE_URL/v1/messages (the URL bashy llm env advertises) returns 404 from the ollama engine. The Anthropic surface lives in pkg/cligw (fleet/CLI gateway) only; the local engine path (pkg/broker dispatch -> proxyEngine) forwards the path unchanged to ollama, which has no /v1/messages. Y3 claims OpenAI + Anthropic + Ollama protocols for local models, so this is a 1.0 gap. Fix: translate Anthropic Messages requests (system, messages, content blocks incl. tool_use/tool_result, max_tokens, temperature, stop_sequences, stream) to the engine's OpenAI-compatible chat API and translate responses back, including SSE streaming event reshaping (message_start, content_block_start/delta/stop, message_delta, message_stop) and usage; reuse pkg/cligw's Anthropic decoding and error types where possible. Tests with a fake engine (non-streaming, streaming, tool use, error mapping), then one live call against a local model through the door.
