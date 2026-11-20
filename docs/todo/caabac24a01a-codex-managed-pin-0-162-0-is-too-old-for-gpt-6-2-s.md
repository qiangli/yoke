---
id: caabac24a01a
kind: bug
title: 'codex managed pin (0.162.0) is too old for GPT-6.2: server gates gpt-6.2-sol/-astra by client version'
seq: 3132
status: todo
priority: p1
labels:
    - agents
created: 2026-11-19T21:45:52.794373Z
---

Seeded by Sprint 671 T6 (a089a922) from codex's models_cache, never live-verified. 2026-11-19: weave run 68 launched codex-gpt6.2-sol headless and the first turn failed: 400 invalid_request_error 'The gpt-6.2-sol model is not supported when using Codex with a ChatGPT account.' (code model_not_found). Check gpt6.2-astra and gpt6.1-terra the same way (bashy agent verify --live), then either mark the bindings API-only (kind/plan) or drop them for ChatGPT-login hosts; golden.txt already notes the same class for codex-gpt5.4-mini. Note the ranking anchor 'GPT-6.2 Astra/Sol' was by public data, not by this binding.

Update 2026-11-19 13:48 local: reproduced in the REAL HOME with both /opt/homebrew/bin/codex 0.157.1 and bashy's managed codex 0.162.0 — same 400, plus 'Model metadata for gpt-6.2-sol not found'. The freshly refreshed ~/.codex/models_cache.json no longer lists gpt-6.2-sol or gpt-6.2-astra (now: gpt-6.1-terra p1, gpt-6.1-sol p3, gpt-6-sol p5, gpt-5.6-sol p7), whereas Sprint 671 T5 read gpt-6.2-sol at priority 1 from the same cache at ~18:12Z. So the server withdrew / has not rolled out GPT-6.2 for this ChatGPT-login account; not a binding typo or binary age. An interactive codex session that started earlier still offers '/model gpt-6.2-sol' from its stale list. Action: re-check before any change (the rollout may return); meanwhile prefer codex-gpt6.1-sol for codex work.

ROOT CAUSE (2026-11-19 13:50, confirmed from ~/.codex/sessions rollouts): the model list and acceptance are gated by Codex CLIENT VERSION. The operator's interactive codex-tui is cli_version 0.170.0 and its catalog fetch (21:50:03Z) lists gpt-6.2-sol p1 / gpt-6.2-astra p2; codex_exec 0.157.1 (Homebrew, first on PATH) and 0.162.0 (bashy managed pin, tools/codex.yaml) fetch a catalog WITHOUT 6.2 (21:48Z, 21:50:35Z) and get 400 model_not_found ('not supported when using Codex with a ChatGPT account' — misleading wording). The id gpt-6.2-sol is correct (= ~/.codex/config.toml). Fix: bump the managed codex pin in yoke pkg/fleet/baseline/tools/codex.yaml to >= 0.170.0 with verified per-platform sha256s, then bashy agent verify --live codex-gpt6.2-sol and codex-gpt6.2-astra. Also: 'codex' on PATH resolves to Homebrew 0.157.1 — bindings must use the managed path.

Shortcut test 2026-11-19 (bashy f151dfb9 + yoke f4a1c2f2 test build): 'bashy codex' launched the MANAGED ~/Library/Caches/bashy/bin/codex/0.162.0 (no --model, so the operator's config gpt-6.2-astra) and the first turn failed 400 model_not_found; launch, session id and steer delivery all worked. By hand the operator runs codex >=0.170 (Codex app daemon 0.171.0), so until this pin is bumped 'bashy codex' differs from 'codex by hand'.
