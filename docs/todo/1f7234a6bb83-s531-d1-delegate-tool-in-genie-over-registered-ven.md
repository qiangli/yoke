---
id: 1f7234a6bb83
kind: feature
title: S531.D1 `delegate` tool in genie over registered vendor CLI commands
seq: 2888
status: done
priority: p0
labels:
    - genie
created: 2026-10-29T07:01:15.230273Z
assignee: codex-gpt6-astra
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T17:07:34.175382Z
resolution: fixed
closed_by: s531-manager
---

Spec: dhnt umbrella plan §D0 (decided f7d071321). Slices 1+2 DONE: yoke c2f48343 + d44c0208 (merge fa623dcd).

Slice 3 (this run, yoke pkg/chat only) — bug found in live setup: `bashy delegate --agent genie:<model>` (bashy's first-party agent; fleet tool `ycode`, alias `genie`) is refused by delegateAuth ("no subscription login check for genie"). Genie authenticates its MODEL itself (fleet model api_key_ref resolved from ~/.config/bashy/secrets.map / vault), not through a vendor login, so:
- First-party targets (tool ycode/genie) skip the vendor subscription probe; vendor tools (claude, codex, and any other) keep it, unknown vendor tools stay fail-closed.
- Everything else still applies to genie targets: allow policy (target string as resolved, e.g. genie:dragon-door-codex-gpt6-sol or ycode:<model> — accept either spelling in the allow list for the first-party tool), idle/timeout, metered-env scrub, structured envelope, one ladder event under capability.DelegateIdentity(tool, model) when caller is genie.
- Confirm `--agent genie:<model>` resolves to the genie/ycode launch with that model (fleet model by name, e.g. a local ~/.config/bashy/models/*.yaml entry) and the prompt; if it does not resolve, fix resolution minimally.
- Do NOT reject genie launches for PreserveEnv/CredentialEnvAliases if the genie recipe declares model-credential env: genie's model key is resolved by genie itself; but still never pass metered vendor keys (the scrub stays).
Acceptance: unit red/green with a fake genie binary: genie target with allow -> launches without vendor auth probe, envelope ok; genie target not in allow -> denied; codex target still requires login (existing tests green). `go test ./pkg/chat/` green with BASHY_SPRINT_LEASE_TOKEN/BASHY_MINT_SPRINT unset. Generic commit wording; no hostnames/IPs.
