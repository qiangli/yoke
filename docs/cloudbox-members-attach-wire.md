# Cloudbox members + pair-once + satellite attach — public wire contract

Status: **Wave 0 of the S606/S583 plan (2026-11-09)** — draft for review by the
operator and the owners of the private cloudbox tickets that implement it
(members `a99e88ac`, attach `988ccd26`). S583.1's design references this file.

This is the **public** contract: what a client (yoke `pkg/fleet`, outpost,
bashy) may send and rely on. It names no server internals — storage, signing
keys, handler layout and the web UI are the implementation's business. A
behavior not written here is not promised.

The executable form of this document is the fake in
[`pkg/fleet/cloudboxfake`](../pkg/fleet/cloudboxfake): an `httptest` master
plus one `httptest` satellite per seeded satellite, serving exactly the
endpoints below. Wave 1–3 client stories code against it and need no live
cloudbox. **If this doc and the fake disagree, that is a bug in one of them —
fix both in the same change.**

## 0. The agentic rule

Agents and people reach cloudbox, satellites and outposts **only through the
bashy ecosystem**: small verbs (`--json`, `--dry-run`, `--no-input`) and, for
every owner/member journey, a Bash# skill-script that composes those verbs
(§6). Never an ad hoc `curl`, never a web-UI step. Every endpoint below is
mapped to the verb that drives it (§5); **an endpoint with no verb is a gap to
fix, not an invitation to hand-roll HTTP.**

## 1. Conventions (every endpoint)

**Base URLs.** The master is `$BASHY_CLOUDBOX_URL` (default
`https://ai.dhnt.io`); a satellite's base is the `endpoint` field of its
satellite record. Paths below are relative to the server named in each
heading. All bodies are `application/json; charset=utf-8`; timestamps are
RFC 3339 UTC.

**Authentication.** `Authorization: Bearer <token>`. Four token kinds appear:

| Token | Issued by | Holder | Scopes it may carry |
|---|---|---|---|
| owner token | master (`bashy token`, login) | a person / their agents | `members:read`, `members:write`, `satellites:read`, `hosts:write` |
| host token | master, `POST /api/v1/hosts/pair` | one paired host | `host` |
| attach token | master, `POST /api/v1/satellites/:id/attach` | one host, for one satellite | `attach` (audience = that satellite id) |
| satellite host token | satellite, `POST /api/v1/hosts/attach` | one attached host | `satellite-host` |

An owner token minted **without** a scope list (the legacy owner Bearer
resolved by `ResolveCloud`) carries every owner scope. A scoped token must
carry the scope named per endpoint. Token values are opaque to clients — do
not parse them.

**Status codes.** `200` read/replayed write, `201` created, `204` deleted (no
body), `400` malformed body or unknown field, `401` missing/invalid/expired
token, `403` valid token lacking the scope, `404` not found **or not yours**
(the two are deliberately indistinguishable), `409` state conflict, `410`
expired, `422` well-formed but invalid value, `429` rate limited (honor
`Retry-After`).

**Error envelope.** Every non-2xx response with a body is:

```json
{"code": "insufficient_scope", "error": "token lacks scope members:write", "required_scope": "members:write"}
```

`code` is a stable machine token (list in §7); `error` is human text that may
change. Optional context fields: `required_scope`, `satellite_id`,
`retry_after` (seconds). Clients branch on `code`, never on `error`. This is
the envelope yoke's `provisionAPIError` already decodes.

**Unknown fields.** Request bodies with unknown fields are rejected `400
bad_request` (fail loudly — a typo must not silently do nothing). Response
objects may gain fields; clients must ignore unknown response fields.

**Idempotency.** Every `POST` accepts an optional `Idempotency-Key` header
(≤128 chars). A replay with the same key, same caller, same method+path and
same body within **24 h** returns the original status and body. Same key with
a different body → `422 idempotency_mismatch`. Independently of the header,
each endpoint states its *natural* idempotency below. `PATCH` and `DELETE`
are naturally idempotent.

**Caching.** Every response carries `Cache-Control: no-store` except the two
membership reads, which carry `Cache-Control: private, max-age=30`. Clients
may cache those for up to 30 s; nothing else is cacheable.

**Lists** are unpaginated and bounded (≤ 1000 rows); the envelope is always
`{"<plural>": [...]}` and an empty list is `[]`, never `null`.

## 2. Members (master)

A member row is directional: **owner → member email, filed under a circle**.
`circle` is the owner's private label for that person, from the **closed set
`family` | `friend` | `team` | `guest`**, default `guest` (least privilege).
Any other value is `422 invalid_circle`. (Named teams such as `team:<name>`
are a later follow-up and are not part of this contract.) Circles scope
sharing decisions downstream (e.g. `bashy llm peer --circle`, S583.8 catalog
circles).

```json
{"id": "mem_7f3a", "email": "bob@example.com", "circle": "family",
 "status": "pending", "mutual": false, "invitation_id": "inv_91c2",
 "created_at": "2026-11-09T18:00:00Z", "accepted_at": null}
```

`status` is `pending` (invited, not accepted) or `active`.

**Mutual.** A membership is *mutual* when **both** directional rows exist and
are `active`; every member row carries `"mutual": true|false`. Downstream
membership checks (grants, satellite attach, LLM sharing) **require
`mutual`** — a one-sided or pending row grants nothing. Deleting **either**
row ends the membership (the survivor reads `mutual: false`) and detaches
any host attached to the other owner's satellite under it (§4 timing). A member
leaves by deleting their own reverse row; there is no endpoint for removing
a row someone else owns.

### GET /api/v1/members — scope `members:read`

Query: `circle=<c>` (optional exact filter). → `200 {"members": [Member...]}`.
Cacheable 30 s.

### POST /api/v1/members — scope `members:write`

Body `{"email": "bob@example.com", "circle": "family"}` (`circle` optional,
default `guest`). → `201 {"member": Member, "invitation": Invitation}` with
`status: "pending"`, `mutual: false`. The invitee is notified out of band
(email) — the notification is not part of this contract.

Natural idempotency: if the caller already has a row for that email (any
status) → `200 {"member": Member}` **unchanged** — use PATCH to change the
circle. Errors: `422 invalid_email`, `422 invalid_circle`, `422 self_member`.

### PATCH /api/v1/members/:id — scope `members:write`

Body `{"circle": "team"}` — `circle` is the only mutable field. → `200
{"member": Member}`. `404 not_found`, `422 invalid_circle`.

### DELETE /api/v1/members/:id — scope `members:write`

→ `204`. Removes the caller's row (and cancels its pending invitation). It
does not remove the other person's row, but it **ends the membership**: the
remaining row reads `mutual: false` and passes no membership check. Deleting
an absent id is `204` (idempotent; existence is not revealed).

### GET /api/v1/member/invitations — scope `members:read`

Invitations addressed to the caller's verified email, pending only.
→ `200 {"invitations": [Invitation...]}`.

```json
{"id": "inv_91c2", "from_email": "alice@example.com", "status": "pending",
 "created_at": "2026-11-09T18:00:00Z", "expires_at": "2026-11-16T18:00:00Z"}
```

The inviter's circle label is **not** disclosed to the invitee (least
disclosure). Invitations expire after **7 days**.

### POST /api/v1/member/invitations/:id/accept — scope `members:write`

Mutual accept (**operator decision**): accepting flips the inviter's row to
`active` **and adds the reverse row** — the inviter becomes a member in the
invitee's own list — so both rows are active and both read `mutual: true`.
Body (optional) `{"circle": "friend"}` files the reverse row; the reverse
circle is the invitee's own choice and defaults to `guest`.

→ `200 {"member": Member /* inviter's row, now active */, "reverse": Member /* invitee's new row */}`

Natural idempotency: accepting an already-accepted invitation returns `200`
with the same two rows; the reverse row's circle is not changed by a replay.
If the invitee already had a row for the inviter, it is reused (status set to
`active`, circle untouched). Errors: `404 not_found` (unknown, revoked, or
addressed to someone else), `410 invitation_expired`, `422 invalid_circle`.

### GET /api/v1/members/of — scope `members:read`

The reverse view: rows **other owners** hold for the caller. Carries no
circle — the owner's label is not shown to the member (least disclosure).
→ `200 {"of": [{"id": "mem_7f3a", "owner_email": "alice@example.com",
"status": "active", "mutual": true, "since": "2026-11-09T18:05:00Z"}]}`.
Cacheable 30 s.

## 3. Pair once — identity-only (master)

Pairing establishes **who a host is** and nothing else: it stores a host
record and returns a host token. It opens **no tunnel** and grants no
satellite — reachability is the attach step's job (§4).

### POST /api/v1/hosts/invites — scope `hosts:write` (existing)

→ `201 {"code": "K7QX-2MPD", "expires_at": "…"}`. Single-use, 15 min TTL.

### POST /api/v1/hosts/pair — no Bearer; the code is the credential

Body `{"code": "K7QX-2MPD", "host": "studio", "hostname": "studio.local",
"os": "darwin", "arch": "arm64"}` (`code`, `host` required).

→ `201 {"host": Host, "token": "<host token>", "scopes": ["host"], "tunnel": false}`

```json
{"host": "studio", "hostname": "studio.local", "os": "darwin", "arch": "arm64",
 "owner_email": "alice@example.com", "paired_at": "2026-11-09T18:10:00Z"}
```

`tunnel` is always `false` on this endpoint; a client that sees `true` is
talking to a different contract and must stop. Natural idempotency: the code
is consumed by the first success; a retry with the same code **and the same
`host`** within the code's TTL returns `200` with the same record and the
same token; anything else → `409 code_consumed`. Errors: `401 invalid_code`,
`410 code_expired`, `409 host_exists` (name already paired by another
host — unpair it first).

### DELETE /api/v1/hosts/self — scope `host`

The host unpairs itself: record deleted, host token revoked immediately,
every satellite assignment detached (§4 revocation timing applies to the
satellites). → `204`; repeat calls `401` (the token is gone) — clients treat
that as done.

## 4. Satellite attach

The host asks the master for permission, then redeems it at the satellite.
The host and the satellite must belong to the same owner, or the host's
owner must be a **mutual** member of the satellite's owner (§2).

**One satellite per host is the CURRENT server limit** (a second attach is
`409 attached_elsewhere`), not a contract invariant: the assignment is
exposed as a **list**, and the per-satellite `attached` flag is per row, so
concurrent attachments can come later without a wire change. Clients must
read the list and not assume a single row.

```
host --(host token)--> master  POST /api/v1/satellites/:id/attach   -> attach_token
host --(attach_token)--> sat   POST /api/v1/hosts/attach            -> satellite host token
```

### GET /api/v1/satellites — scope `satellites:read` (owner) or `host` (master)

Existing owner shape `{"satellites": [{id, name, endpoint, state}]}`. With a
**host** token the rows are the satellites the host may attach to — its
owner's, plus those of owners its owner is a **mutual** member of — and each
row gains `"attached": true|false` for that host. A satellite outside that
set is `404 not_found` on every satellite endpoint (a pending or one-sided
membership reveals nothing).

### GET /api/v1/satellites/:id — scope `satellites:read` or `host` (master)

→ `200 {"satellite": {"id", "name", "endpoint", "state", "owner_email",
"hosts": ["studio"]}}`. `hosts` is the attached host names and is present for
owner tokens only; a host token sees `attached` instead. `404 not_found`.

### GET /api/v1/satellites/assignment — scope `satellites:read` or `host` (master)

The host→satellite assignment over Bearer (S5; today the session-only
`/api/satellites/assignment`). Query `satellite=<id>` filters.
→ `200 {"assignments": [{"host": "studio", "satellite_id": "sat_1",
"state": "pending"|"attached", "assigned_at": "…", "attached_at": "…"|null}]}`.
A host token sees only its own row; an owner token sees the rows of its own
hosts and of hosts attached to its own satellites.

### POST /api/v1/satellites/:id/attach — scope `host` (master)

No body. → `201 {"attach_token": "…", "expires_at": "…", "satellite":
{"id", "name", "endpoint"}, "host": "studio"}` and the assignment becomes
`pending`.

The attach token is single-use, **10 min** TTL, audience = this satellite.
Natural idempotency: a repeat POST for the same satellite issues a **new**
token and invalidates any unredeemed earlier one (`201` again); if the host
is already `attached` here it still returns `201` (re-attach refreshes the
satellite host token). Errors: `404 not_found`, `409 attached_elsewhere`
(with `satellite_id` — detach first), `409 satellite_unavailable` (the
satellite is not `enabled`).

### DELETE /api/v1/satellites/:id/attach — scope `host` (master)

Detach: assignment removed, outstanding attach token invalidated, satellite
host token revoked (timing below). → `204`, idempotent.

### POST /api/v1/hosts/attach — attach token (satellite)

Body `{"host": "studio", "hostname": "studio.local", "os": "darwin", "arch":
"arm64"}`; `host` must equal the host named in the attach token.

→ `201 {"host": SatelliteHost, "token": "<satellite host token>", "scopes":
["satellite-host", ...], "satellite_id": "sat_1", "registration": {...}}`.
`registration` is the matrix-tunnel registration the host dials, with the same
fields a legacy `/api/register/exchange` returns (`agent_name`, `server_addr`,
`server_port`, `protocol`, `token`, `remote_port`, `access_token`,
`cloudbox_ticket_pubkey`); the outpost keeps it with the attachment and its one
tunnel client serves the satellite exactly like a legacy pairing. `scopes`
includes the canonical outpost scopes the satellite grants an attached host.
The master assignment becomes `attached`.

Natural idempotency: re-presenting the **same** attach token with the same
body within its TTL returns `200` with the same record and token (safe to
retry a lost response). Errors: `401 invalid_attach_token` (unknown, wrong
audience, superseded or revoked), `401 attach_token_used` (redeemed by a
different request body), `410 attach_token_expired`, `422 host_mismatch`.

### DELETE /api/v1/hosts/self — scope `satellite-host` (satellite)

The host leaves the satellite: satellite record deleted, satellite host token
revoked, master assignment removed. → `204`; repeat → `401`.

### Revocation timing

| Event | Effective at master | Effective at satellites / peers |
|---|---|---|
| `DELETE /members/:id` | immediately | ≤ 30 s (membership cache) |
| invitation expiry | at `expires_at` | — |
| `DELETE /hosts/self` (master) | immediately | ≤ 60 s |
| `DELETE /satellites/:id/attach` | immediately | ≤ 60 s |
| `DELETE /hosts/self` (satellite) | ≤ 60 s | immediately |
| attach token | single-use; 10 min TTL | — |

"≤ 60 s" is a ceiling a client may rely on: after it, a revoked token gets
`401` everywhere. Clients must not assume faster.

## 5. Endpoint → bashy verb

Two layers, no duplicates: **`outpost …` is the local daemon** acting with
its host token (reached as `bashy outpost …`); **`bashy …` is the account**
acting with the owner token. Existing verbs are reused, not re-invented. All
verbs take `--json`; every mutating verb takes `--dry-run` (prints the
request it would send, sends nothing) and `--no-input`. **Gap** = the verb or
flag does not exist yet; its wave story builds it against the fake.

| Endpoint | Verb | State |
|---|---|---|
| `GET /api/v1/members` | `bashy member ls [--circle C]` | gap (W1) |
| `POST /api/v1/members` | `bashy member invite EMAIL [--circle C]` | gap (W1) |
| `PATCH /api/v1/members/:id` | `bashy member set ID\|EMAIL --circle C` | gap (W1) |
| `DELETE /api/v1/members/:id` | `bashy member rm ID\|EMAIL` (also how a member leaves: remove your reverse row) | gap (W1) |
| `GET /api/v1/member/invitations` | `bashy member invitations` | gap (W1) |
| `POST /api/v1/member/invitations/:id/accept` | `bashy member accept ID [--circle C]` | gap (W1) |
| `GET /api/v1/members/of` | `bashy member of` | gap (W1) |
| members reads (mutual, circle/email filter) | `bashy llm peer --email E` / `--circle C` | gap (W3) |
| `POST /api/v1/hosts/invites` (master) | `bashy paired-host invite` (pairing code for your own account) | gap (W2) |
| `POST /api/v1/hosts/pair` | `outpost register --code CODE` — identity-only when the server is the master | exists — W2 handles the `tunnel: false` response |
| `DELETE /api/v1/hosts/self` (master) | `outpost unpair` | client method implemented; W2 wires outpost unpair to this endpoint |
| `GET /api/v1/satellites` (owner) | `bashy satellite list` | exists |
| `GET /api/v1/satellites` (host) | `outpost satellite list` | gap (W2) |
| `GET /api/v1/satellites/:id` | `bashy satellite show ID` | implemented — direct ID lookup; unique names resolve through the list on 404 |
| `GET /api/v1/satellites/assignment` | `bashy paired-host ls --satellite ID` | implemented — filters hosts and peers using the Bearer assignment list |
| `POST /api/v1/satellites/:id/attach` + satellite `POST /api/v1/hosts/attach` | `outpost satellite attach ID` (one verb, both calls) | gap (W2) |
| `DELETE /api/v1/satellites/:id/attach` + satellite `DELETE /api/v1/hosts/self` | `outpost satellite detach ID` | gap (W2) |

## 6. Journeys as Bash# skill-scripts

Each journey is a skill-script that composes the verbs above — no HTTP,
no browser. Shapes (names are the W1–W3 deliverables):

- **owner invites a member** — `bashy member invite bob@example.com --circle
  family --dry-run` → confirm → `bashy member invite … --json` → `bashy
  member ls --circle family`.
- **member accepts** — `bashy member invitations --json` → pick by
  `from_email` → `bashy member accept ID --circle friend` → `bashy member of`
  shows the owner with `mutual: true`, `bashy member ls` shows the reverse row.
- **member leaves** — `bashy member ls --json` → `bashy member rm ALICE`
  (the reverse row) → the owner's row reads `mutual: false`.
- **host joins a satellite** — owner: `bashy paired-host invite` → on the
  host, once: `bashy outpost register --code CODE` → `bashy outpost
  satellite list --json` → `bashy outpost satellite attach ID` → owner
  checks with `bashy paired-host ls --satellite ID`.
- **host leaves** — `bashy outpost satellite detach ID`; to forget the host
  entirely, `bashy outpost unpair`.
- **share an LLM with a circle** — `bashy member ls --circle family` →
  `bashy llm peer --circle family` (mutual members only).

## 7. Error codes

`bad_request`, `unauthorized`, `insufficient_scope`, `not_found`,
`invalid_email`, `invalid_circle`, `self_member`, `invitation_expired`,
`idempotency_mismatch`, `invalid_code`, `code_expired`, `code_consumed`,
`host_exists`, `attached_elsewhere`, `satellite_unavailable`,
`invalid_attach_token`, `attach_token_used`, `attach_token_expired`,
`host_mismatch`, `rate_limited`.

## 8. Review decisions (2026-11-10)

1. **Leave from the reverse side** — no `DELETE /api/v1/members/of/:id`. A
   member leaves by deleting their own reverse row; since membership checks
   require `mutual`, that ends it.
2. **Owner-initiated assignment** (pre-assign) — out of scope; only the host
   starts an attach.
3. **Circle visibility** — least disclosure: `members/of` rows and
   invitations carry the owner/from email, status, mutual and since, never
   the owner's circle.
4. **Circles** — closed set `family|friend|team|guest`, default `guest`;
   `team:<name>` is a later follow-up.
5. **One satellite per host** — current server limit, not an invariant; the
   assignment stays a list.
