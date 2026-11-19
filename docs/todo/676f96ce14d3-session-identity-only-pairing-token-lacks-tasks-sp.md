---
id: 676f96ce14d3
kind: bug
title: 'session: identity-only pairing token lacks tasks/sprints scopes; scope error hint is stale'
seq: 3130
status: todo
priority: p2
labels:
    - cloudbox
created: 2026-11-19T19:19:35.372396Z
---

Seen 2026-11-19 on the dev box (macOS, outpost v1.0.0-rc.37-dev, 'paired once as dragon (identity only)'). bashy sprint session status / inbox --peek seat join fail: 'insufficient scope: this host's cloudbox token lacks tasks:read ... once cloudbox runs a build with them (tasks:*/sprints:*, Sprint 217) and its boot-time scope catch-up has run'. Root cause: with no CLOUDBOX_TOKEN/BASHY_FLEET_TOKEN/BASHY_API_KEY set, pkg/weave resolveSessionCredentials -> fleet.ResolveCloud -> pairedToken (outpost token print), which for an identity-only pairing is a host-scoped token (bashy token list: host-<uuid>, scope 'host'). Identity-only pairing never gains tasks:*/sprints:*, so the Sprint 217 catch-up hint (pkg/weave/cloudbox_client.go:456) is stale and waiting does not help. Classification: product defect (stale guidance + no scoped credential path for paired hosts). Fix options: the paired-host path obtains a scoped session credential (e.g. satellite attachment or a minted coord token with sprints:*), or the error names the real remedy (mint tasks:read,tasks:write,sprints:read,sprints:write via bashy token create --to-secret and set CLOUDBOX_TOKEN). Workaround applied on this host (2026-11-19): token 'session-dragon' (sprints:read,sprints:write,tasks:read,tasks:write) minted in the cloudbox console, value piped straight into vault secret cloudbox-session-dragon, and CLOUDBOX_TOKEN=@cloudbox-session-dragon added to the secrets map for interactive shells. Verified: `bashy secret exec cloudbox-session-dragon=CLOUDBOX_TOKEN -- bashy sprint session status` resolves the session, and the inbox seat join reports 'synced'. Second gap: agent (non-interactive) shells cannot use `bashy secret env` (reveal refused by design), so agent seat sync needs the secret exec wrapper. The real fix should let the session client resolve a scoped credential by handle (vault reference) instead of a plain env var.
