---
id: 76128d5f52c2
kind: bug
title: model quota --refresh fails (analyzer door 502) on hosts without a local model
seq: 2030
status: done
labels:
    - quota
created: 2026-10-19T02:11:41.108803Z
assignee: agy-gemini3.8-flash
sprint: 469
sprint_id: ce3c3f37-86a2-5069-a2b7-043f2f9be051
sprint_title: Bashy model quota accuracy fixes
closed: 2026-10-19T05:36:26.690037Z
closed_by: codex-gpt6-sol
---

Observed 2026-10-18 on Dragon: bashy model quota --refresh prints 'analyzer door status 502'. bashy llm status shows backend ollama-local present=true healthy=false (no local models on this host by policy) and cligw healthy. The refresh path depends on a local-model analyzer, so quota can never be refreshed on a model-free host; this blocked Sprint 446 S10/S21 vendor arms all afternoon. Expected: refresh falls back to deterministic parsing or routes the analyzer through a healthy backend, and reports which sources were refreshed versus skipped instead of a bare 502. Fix with a test on a host with no local backend.
