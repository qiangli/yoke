---
id: 1da1b9c67cf7
kind: bug
title: 'delegate: claude needs a vault token its interactive login does not provide'
seq: 3158
status: todo
priority: p2
created: 2026-11-27T14:25:31.863963Z
---

Found 2026-11-27 (Sprint 738 §1). bashy delegate --agent claude-opus5.5 fails with 'worker claude has no subscription credential for vault ref claude-code-oauth-token' on a host whose claude CLI is logged in interactively (weave fleet --probe reports USABLE). delegateAuth (pkg/chat/delegate_guard.go) -> fleet.PrepareWorkerEnv -> secrets.ResolveAgentKey looks only for CLAUDE_CODE_OAUTH_TOKEN* in the env, a secrets.map binding, or the secrets-env cache; the interactive login never counts. Same failure with or without a sandbox. Fix direction: either accept the CLI's own login when the token is absent, or make the refusal name the exact setup (claude setup-token, store in the vault, bind in secrets.map) and add a one-command helper. Not a Sprint 738 story.
