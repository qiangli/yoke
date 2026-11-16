# Plan: self-service LLM keys for circle members (`bashy llm key`)

**Epic:** `lern-psic` · **Track:** teacher/tools · **Status:** backlog (not scheduled)
**Requested:** 2026-11-15, from the psic2027 classroom (Week 4 session).
**Home:** yoke, beside the other model-door commands (`pkg/broker`: `peer`, `share`, `usage`); surfaced as `bashy llm key` like them.

## Problem

A shared model door (for psic2027: Elephant's door, published through the
`lern-si` satellite) is reached with one personal peer credential per user.
Today the owner mints each credential (`bashy llm peer add`), prints it once,
and delivers it out of band (paper slips in class). That does not scale, is
easy to leak, and ties nothing to the person's real identity unless the owner
also runs `bashy llm peer set NAME --email E --circle C` by hand.

## Goal

Any person who is **signed in to Cloudbox**, an **active mutual member** of
the door owner's account, in an **allowed circle**, and who can reach the
door's `llm` app, gets their own credential with one command — no owner
step, no out-of-band delivery:

```sh
bashy login                              # once: Cloudbox sign-in
bashy llm key --satellite lern-si        # mint (or rotate) my key; stored, never printed
bashy llm key --satellite lern-si --env  # print client env lines (key via file reference)
```

## Prerequisites on the door host

1. The model door is running: `bashy llm up`.
2. The `llm` app is published through the outpost instance attached to the
   satellite: `outpost-profile up <satellite> --app llm`. (For psic2027 on
   2026-11-15: Elephant, profile `lern-si`, done and verified.)
3. Self-service is opted in by the owner, naming who may mint:
   `bashy llm self-service on --circle team2027 [--circle …] [--tokens-day N] [--usd-month X] [--rpm N] [--soft]`.
   Off by default; `off` stops new mints without touching existing keys.

## Design

**Identity.** The mint request must carry the caller's Cloudbox identity as
the satellite asserts it (the same signed `Remote-User` stamp outposts
already pass to `require_login` + `trust_cloud_identity` apps such as
Klatch). The inference route keeps working without sign-in (Bearer key), so
the mint route needs a sign-in-required entry of its own — e.g. a second app
entry `llm-key` (require_login, trust_cloud_identity) pointing at the same
door, or a per-path login rule if the outpost supports one.

**Mint.** `POST /v1/peer/self` on the door:

- verifies the satellite identity signature and freshness;
- checks the email is an active mutual member in an allowed circle, with
  the existing `MemberAuthorizer` (`pkg/broker/peershare.go`);
- mints or rotates the peer `self-<normalized-email>` with
  `MintPeerCredentialFor(..., share=allow, email, circle)`, so every later
  inference request re-checks membership (leaving the circle disables the key);
- applies the owner's self-service defaults (`peer limit`), or none when `--soft`;
- returns the credential once; logs only the principal, never the value.

Rerunning rotates (the old value stops working). The owner can still
`revoke`, `share deny`, or `limit` any self-service peer by name.

**CLI.** `bashy llm key --satellite NAME [--door HOST]`:

- calls the mint route with the caller's Cloudbox session (no credential in argv);
- stores the key owner-only (and in the vault when signed in), never printing it;
- `--env` prints client settings that reference the key by file, e.g. for
  OpenCode `apiKey: "{file:…}"`;
- exit codes: not signed in, not a member, circle not allowed, self-service
  off, door unreachable — each with one line naming the fix.

**Limits.** Caps stay per credential (`peer limit`: tokens/day, USD/month,
rpm; hard, 429 over cap). With one self-service key per email, per-key equals
per-person. `--soft` sets no caps; `bashy llm usage` reports per principal, so
the owner reviews `peer:self-*` rows for misuse.

## Open questions (resolve before scheduling)

1. **Does the satellite stamp a signed identity on CLI (non-browser) requests**
   made with a `bashy login` session? If not, this needs a Cloudbox support
   ticket — satellites are not shaped locally.
2. Separate `llm-key` app entry vs. a per-path login rule on the existing `llm` app.
3. Principal naming (`self-<email>`) vs. reusing an existing owner-minted peer
   for the same email (psic2027 already has `psic-<login>` peers): migrate or
   coexist?
4. Whether a pending (not yet accepted) member should get a clear "accept your
   invitation" error rather than a generic refusal.

## Acceptance

1. With self-service on for circle `C`, a signed-in active member of `C`
   runs `bashy llm key --satellite S` and then completes a chat request through
   S with the stored key; the owner sees `peer:self-<email>` in `bashy llm usage`.
2. A non-member, a pending member, a member of another circle, and an
   anonymous caller are each refused with a distinct one-line reason; nothing
   is minted.
3. Removing the member from `C` makes their next inference request fail
   (membership re-checked per request) without the owner touching the peer.
4. Rerunning `bashy llm key` rotates: the previous value returns 401.
5. The credential never appears in argv, logs, or command output (only with
   an explicit `--print`).
6. `self-service off` refuses new mints; existing keys keep working until revoked.

## Stories (draft)

- door: `POST /v1/peer/self` — satellite identity, circle check, mint/rotate, defaults.
- door: `bashy llm self-service on|off|status` — owner opt-in, circles, default caps.
- outpost/satellite: sign-in-required entry for the mint route (or per-path login); verify CLI identity stamping (open question 1).
- cli: `bashy llm key` — call, store owner-only, `--env`, error lines.
- classroom: psic2027 `skills/opencode-bedrock` `setup` uses `bashy llm key` instead of the paper-slip prompt.
- docs: llm reference + e2e on a shared test host (member, non-member, rotation, removal).

## Classroom context

psic2027 today (2026-11-15): Elephant door via `lern-si`; ten owner-minted
peers `psic-<login>`, no caps (soft limits by usage review); keys delivered on
printed slips; `team2027` circle has 11 members, 3 active and 8 pending. See
`lern-si/psic2027` `docs/llm-reference.md`.
