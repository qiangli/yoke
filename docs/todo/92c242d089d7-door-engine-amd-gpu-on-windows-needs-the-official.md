---
id: 92c242d089d7
kind: feature
title: 'door engine: AMD GPU on Windows needs the official Ollama; make the engine command configurable instead of BASHY_SELF'
seq: 3101
status: todo
priority: p2
created: 2026-11-11T17:16:55.586533Z
---

Sprint 613 on a Ryzen AI Max+ 395 (Radeon 8060S) Windows host: the embedded signed runner offloads 0/25 layers (no Vulkan/ROCm); gpt-oss:20b took ~99 s per short reply and opencode timed out. Workaround used: BASHY_SELF pointing at a .cmd that maps 'ollama serve' to the official Ollama v0.40.16 (zip + rocm zip): 25/25 layers on GPU, ~135 tok/s, opencode coding turn in 17 s. Wanted: a supported --engine-cmd (or engine registry entry) for the door's local backend.
