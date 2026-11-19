---
id: 676f96ce14d3
kind: bug
title: 'session: identity-only pairing token lacks tasks/sprints scopes; scope error hint is stale'
seq: 3130
status: done
priority: p2
labels:
    - cloudbox
created: 2026-11-19T19:19:35.372396Z
closed: 2026-11-19T19:32:09.18933Z
---

Seen 2026-11-19 on the dev box (macOS, outpost v1.0.0-rc.37-dev, 'paired once as <host> (identity only)'). bashy sprint session status / inbox --peek seat join fail: 'insufficient scope: this host's cloudbox token lacks tasks:read ... once cloudbox runs a build with them (tasks:*/sprints:*, Sprint 217) and its boot-time scope catch-up has run'. Root cause: with no CLOUDBOX_TOKEN/BASHY_FLEET_TOKEN/BASHY_API_KEY set, pkg/weave resolveSessionCredentials -> fleet.ResolveCloud -> pairedToken (outpost token print), which for an identity-only pairing is a host-scoped token (bashy token list: host-<uuid>, scope 'host'). Identity-only pairing never gains tasks:*/sprints:*, so the Sprint 217 catch-up hint (pkg/weave/cloudbox_client.go:456) is stale and waiting does not help. Classification: product defect (stale guidance + no scoped credential path for paired hosts). Fix options: the paired-host path obtains a scoped session credential (e.g. satellite attachment or a minted coord token with sprints:*), or the error names the real remedy (mint tasks:read,tasks:write,sprints:read,sprints:write via bashy token create --to-secret and set CLOUDBOX_TOKEN). Workaround applied on this host (2026-11-19): token 'session-<host>' (sprints:read,sprints:write,tasks:read,tasks:write) minted in the cloudbox console, value piped straight into vault secret cloudbox-session-<host>, and CLOUDBOX_TOKEN=@cloudbox-session-<host> added to the secrets map for interactive shells. Verified: `bashy secret exec cloudbox-session-<host>=CLOUDBOX_TOKEN -- bashy sprint session status` resolves the session, and the inbox seat join reports 'synced'. Second gap: agent (non-interactive) shells cannot use `bashy secret env` (reveal refused by design), so agent seat sync needs the secret exec wrapper. The real fix should let the session client resolve a scoped credential by handle (vault reference) instead of a plain env var.

## Resolution (2026-11-19)

Fixed in yoke b9d31108: the session token variable ($CLOUDBOX_TOKEN, or the pointer's TokenRef) resolves in-process from the host's secrets.map binding when the environment lacks it (new secrets.BoundValue; env still wins), so agent shells sync without `secret exec`. The insufficient-scope error now names the real remedy (mint tasks:*/sprints:* and bind CLOUDBOX_TOKEN=@<vault name>). Verified end to end from an agent shell with no CLOUDBOX_TOKEN: `sprint session status` resolves and the inbox seat join reports synced. Remaining by design: each identity-only paired host needs one minted session token plus its binding; automatic minting is not possible (a pairing token cannot mint scopes it lacks).
