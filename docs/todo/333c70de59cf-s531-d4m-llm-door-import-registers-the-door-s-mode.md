---
id: 333c70de59cf
kind: task
title: S531.D4m llm door import registers the door's models for genie/HOME-only agents (no hand-written models yaml/secrets.map)
seq: 2928
status: done
priority: p0
created: 2026-10-29T18:01:31.922234Z
assignee: codex-gpt6-astra
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T18:46:46.099963Z
closed_by: s531-manager
---

Found in D3 probe 10-29 on elephant WSL with a fresh HOME. After 'eval $(bashy llm env --from dragon)', 'bashy genie -m codex-gpt6-sol prompt' tries 'pulling codex-gpt6-sol' from local ollama and fails: genie ignores OPENAI_BASE_URL for a bare served id. The shared setup instead relied on hand-written ~/.config/bashy/models/dragon-door-<id>.yaml (provider openai-compat, base_url, api_key_ref dragon-door) plus secrets.map DRAGON_DOOR_API_KEY. Those went stale when llm share rotated the credential, which blocked S525 A9. Fix (zero user setup, same on Windows/WSL/Linux/macOS): llm door import (and a rerun of share, which rotates) records the door as a model source genie resolves, e.g. '<door>-door-<served id>' or '<id>@<door>', reading the credential from the door store at call time, so rotation never strands a copy. A HOME-only agent with no env works. WSL with no grant of its own resolves through the Windows store like llm env does. Red/green unit tests in pkg/broker + the genie model resolver; focused go test only.
