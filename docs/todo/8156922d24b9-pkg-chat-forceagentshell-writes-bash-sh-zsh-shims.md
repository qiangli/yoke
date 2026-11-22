---
id: 8156922d24b9
kind: bug
title: 'pkg/chat forceAgentShell writes bash/sh/zsh shims from os.Executable(): under a command adapter (foreman) the shims exec the adapter binary instead of bashy'
seq: 3150
status: done
priority: p0
labels:
    - yoke
    - adapter
created: 2026-11-22T14:02:40.007769Z
weave: 97
assignee: codex-gpt6-astra
sprint: 692
closed: 2026-11-22T14:07:01.594382Z
resolution: fixed
closed_by: codex-gpt6-astra-s
---

Second half of bashy 3d47facb (install-agent side fixed in bashy eb1a0a9f). Found by codex 2026-11-22 in yoke pkg/chat/chat.go around lines 845-847: the forceAgentShell branch calls os.Executable() and then ensureShims -> WriteShellShim. When the launching process is the yoke/foreman adapter binary (bashy foreman -> adapter), os.Executable() is the adapter under the adapter cache, and the shims written to ~/.bashy/shims exec foreman. Symptom on the dev box: every '#!/usr/bin/env bash' script ran under foreman. Fix (smallest): resolve the shell from the bashy launcher identity (BASHY_SELF when set, else the bashy on PATH), never os.Executable() when it lies under the adapter cache (BASHY_ADAPTER_CACHE, XDG_CACHE_HOME/bashy/adapters, ~/.cache/bashy/adapters); refuse to write a shim whose target is under the cache. Unit test in pkg/chat: with BASHY_SELF set and a fake os.Executable under a temp adapter cache, ensureShims writes shims that exec BASHY_SELF, and refuses a cache target. Owner codex, --repo yoke. TEST RULE (operator 2026-11-22): build + smallest targeted unit tests (pkg/chat) only, then submit; the one full run is the rc.40 candidate gate 458628c0. Start on current origin main; Sprint/Story/Story-ID + Agent trailers; yoke commit+push first, then the conductor bumps bashy's yoke pin.
