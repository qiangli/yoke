---
id: 7171c4f64887
kind: feature
title: 'S531.D4y one-step llm door share: bashy llm share PEER + bashy llm env --from HOST --via mesh|cloudbox (yoke impl of umbrella 981aee6a)'
seq: 2920
status: done
priority: p0
created: 2026-10-29T16:59:34.259506Z
assignee: claude-opus5.5
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T17:45:33.863684Z
closed_by: s531-manager
---

Operator 2026-10-29: keep it simple. bashy only needs to expose the llm door (cligw) over the network; other tools just use it with a base URL and key. No new routing or tunnel framework; Sprint 532 was retired as over-engineered. Today: mesh exposure works but takes four manual steps (outpost mesh service add llm; bashy llm env --via mesh on the door host; move the token; outpost mesh dial on the client). --via has no cloudbox option. Fix: (1) one command on the door host shares it with an owned peer (wrap mesh service add plus llm peer add/share); (2) one command on the client, e.g. bashy llm env --from HOST [--via mesh|cloudbox], dials and obtains the peer credential over the already-authenticated owner channel, with no manual token transfer, and prints the usual OpenAI/Anthropic env; (3) --via cloudbox uses the existing satellite app route; the cloudbox front strips Authorization, so use the existing /k/<token> base-URL form. Same on Windows/macOS/Linux; vendor logins stay on the door host; no metered keys. Acceptance: elephant (native Windows and WSL) uses Dragon's door over mesh with two commands total, a tool (codex or genie) completes a request, and one request succeeds via cloudbox. This replaces the hand-wired elephant setup.

## Manager design (s531-manager 2026-10-29, binding for the worker; yoke pkg/broker only unless noted)

Facts checked live: the door already admits a peer credential via bearer AND via the /k/<cred> path form (pkg/broker/broker.go authorize/splitParams); peers reach inference routes only (peerRouteAllowed). Door host -> peer `outpost ssh` is passwordless on owned hosts; peer -> door host `outpost ssh` asks for an OS password (not usable). So the credential is PUSHED by the door host over the owned outpost-ssh channel; the client never needs a pasted token.

1. Door host: `bashy llm share PEER [--via mesh|cloudbox|both]` (default mesh) — one command, idempotent:
   - mesh: ensure `outpost mesh service add llm 127.0.0.1:<door port>` (skip if present).
   - cloudbox: ensure the door is a satellite custom app named `llm` (`outpost apps add`, upstream = door loopback, public/anon, no SSO) so `https://<cloudbox>/matrix/h/<doorhost>/app/llm/` reaches it; record the resulting base URL.
   - mint (or rotate) peer credential PEER with share=allow (MintPeerCredential).
   - deliver it to PEER over the owned outpost ssh path (yoke sshclient / same mechanism as `outpost ssh user@host -- cmd`), feeding a small JSON on STDIN (never argv) to `bashy llm door import` on the peer. Record: door host name, mesh peer id + service name, cloudbox base URL (if any), credential. No credential ever printed or logged on the door host.
   - `--print` fallback (stdout JSON) only for hosts that the door cannot reach; not the default.
2. Peer store: `$BASHY_HOME/llm/doors/<doorhost>.json` (else ~/.bashy/...), 0600 on unix; on Windows set an owner-only ACL (or reuse whatever pkg/secret / toolcmd peers.json uses on Windows) — same code path on all OSes. Also upsert the toolcmd peers.json entry for that peer so `tool cmd run --peer` works without a hand-written file.
3. Client: `bashy llm env --from HOST [--via mesh|cloudbox]` (existing env command, new flag):
   - reads the store; missing -> clear error "run `bashy llm share <thishost>` on HOST" (exit non-zero).
   - mesh: ensure a persistent forward (`outpost mesh consume add llm <peerid> --local ADDR`, idempotent; reuse if present) and use ADDR as base. cloudbox: base = recorded cloudbox URL + `/k/<cred>` (front strips Authorization, so the key MUST be in the path; also set *_API_KEY to the cred for clients that need a non-empty key).
   - prints the same generic/openclaw/codync/--json blocks as today, with OPENAI/ANTHROPIC base + key = peer credential. Never the owner token.
   - also allow models listing for peers: permit GET /v1/models for shared peers in peerRouteAllowed (inference-only otherwise unchanged) so the client can discover served ids (bug #6 of todo 63cbc00f).
4. WSL (same rule as everywhere, zero manual steps): when running inside WSL with no store of its own, read the Windows user's store (interop: `cmd.exe /c echo %USERPROFILE%` -> wslpath) and for mesh obtain the forward from the Windows side bound to an address WSL can reach (the WSL host gateway address, e.g. the vEthernet(WSL) IP from `ip route` default gw) — do this by calling the Windows bashy (`bashy.exe llm env --from HOST --via mesh --json --bind-for-wsl`) via interop, so the Windows outpost owns the forward. If interop is off, fall back to an explicit error naming `--via cloudbox`. Keep this a small helper, not a framework.
5. Remove the help text describing the manual 4-step mesh recipe; document the two commands.

Tests (red/green, no network): share writes the record + rotates; import store perms; env --from renders mesh and cloudbox (/k/ form) blocks from a fake store; peer GET /v1/models allowed, POST to non-inference still 403; WSL helper with a fake interop runner. `go test ./pkg/broker/` green with BASHY_SPRINT_LEASE_TOKEN/BASHY_MINT_SPRINT unset. Generic commit wording; no hostnames/IPs in code, tests or docs (use dooor/peer placeholders).
Out of scope: no routing framework, no discovery service, no cloudbox code change (if the satellite app route cannot serve it unchanged, stop and report).
