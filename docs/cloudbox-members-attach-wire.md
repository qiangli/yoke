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
`circle` is the owner's private label for that person: **any owner-chosen
lowercase slug `[a-z0-9-]` of 1–32 characters**, default `guest` (least
privilege). `family` | `friend` | `team` | `guest` are the **default
suggestions**, not a closed set; `public` and `owner` are reserved. A
malformed or reserved value is `422 invalid_circle` with an actionable
reason. There is no circle table: a circle exists while some member carries
it; listings offer the defaults plus names in use. Circles scope sharing
decisions downstream (e.g. `bashy llm peer --circle`, S583.8 catalog
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

## 2b. Host app grants

The host owner creates one rule per share. A rule is
`{"circles":[],"groups":[],"emails":[]}`; every list is an array, even when
empty. The owner always passes. A non-owner must be an active mutual member
and match `circles AND (groups OR emails)`: an empty circles list does not
restrict circles, and when both groups and emails are empty, membership and
circle alone suffice. A person passes if any grant for the host and app
matches. An empty grant list admits the owner only.

- `GET /api/v1/hosts/:host/shares` → `200 {"shares":[{"id","host","app","circles","groups","emails","created"}]}`.
- `POST /api/v1/hosts/:host/shares` body `{"app":"board","circles":[],"groups":[],"emails":[]}` → `201 {"share":{...}}`.
- `DELETE /api/v1/hosts/:host/shares/:id` → `204`.
- `GET /api/v1/hosts/:host/apps/:app/who` → `200 {"host","app","owner","emails":[...]}`; `emails` is the sorted list of admitted non-owner members.

The CLI uses `bashy share add HOST/APP --circle C --group G --email E`
(repeatable flags), `bashy share ls HOST`, `bashy share rm HOST/APP ID`, and
`bashy share who HOST/APP`. It sends one rule per `add` invocation.

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

A host may attach to several satellites. Each attachment is identified by the
host and satellite together. The assignment endpoint returns a list, and
`attached` is evaluated separately for each satellite.

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
A host token sees all rows for its host; an owner token sees the rows of its
own hosts and of hosts attached to its own satellites.

### POST /api/v1/satellites/:id/attach — scope `host` (master)

No body. → `201 {"attach_token": "…", "expires_at": "…", "satellite":
{"id", "name", "endpoint"}, "host": "studio"}` and the assignment becomes
`pending`.

The attach token is single-use, **10 min** TTL, audience = this satellite.
Natural idempotency: a repeat POST for the same satellite issues a **new**
token and invalidates any unredeemed earlier one (`201` again); if the host
is already `attached` here it still returns `201` (re-attach refreshes the
satellite host token). Errors: `404 not_found`, `409 satellite_unavailable` (the
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
| `DELETE /members/:id` | immediately | ≤ 60 s (satellites); ≤ 30 s plus one request (bashy member cache, LLM door) |
| invitation expiry | at `expires_at` | — |
| `DELETE /hosts/self` (master) | immediately | ≤ 60 s |
| `DELETE /satellites/:id/attach` | immediately | ≤ 60 s |
| `DELETE /hosts/self` (satellite) | ≤ 60 s while the master is reachable (durable retry until acknowledged) | immediately |
| attach token | single-use; 10 min TTL | — |

"≤ 60 s" is a ceiling a client may rely on **while the master is reachable**:
satellites pull authorization (members, reverse rows, attachments) every
30 s on their own schedule, independent of checkpoints and release retries.
When the master is unreachable, a satellite (like the LLM door) keeps using
its last successfully pulled authorization for at most **24 h**, records its
age, and after that refuses member access to shared apps and new attaches;
the satellite owner keeps access. Clients must not assume faster than these
ceilings.

## 5. Endpoint → bashy verb

Two layers, no duplicates: **`outpost …` is the local daemon** acting with
its host token (reached as `bashy outpost …`); **`bashy …` is the account**
acting with the owner token. Existing verbs are reused, not re-invented. All
verbs take `--json`; every mutating verb takes `--dry-run` (prints the
request it would send, sends nothing) and `--no-input`. **Gap** = the verb or
flag does not exist yet; its wave story builds it against the fake.

| Endpoint | Verb | State |
|---|---|---|
| `GET /api/v1/members` | `bashy member ls [--circle C]` | implemented (bashy member, S583.2) |
| `POST /api/v1/members` | `bashy member invite EMAIL [--circle C]` | implemented (bashy member, S583.2) |
| `PATCH /api/v1/members/:id` | `bashy member set ID\|EMAIL --circle C` | implemented (bashy member, S583.2) |
| `DELETE /api/v1/members/:id` | `bashy member rm ID\|EMAIL` (also how a member leaves: remove your reverse row) | implemented (bashy member, S583.2) |
| `GET /api/v1/member/invitations` | `bashy member invitations` | implemented (bashy member, S583.2) |
| `POST /api/v1/member/invitations/:id/accept` | `bashy member accept ID [--circle C]` | implemented (bashy member, S583.2) |
| `GET /api/v1/members/of` | `bashy member of` | implemented (bashy member, S583.2) |
| members reads (mutual, circle/email filter) | `bashy llm peer add NAME --email E [--circle C]` / `bashy llm peer set NAME --email E [--circle C]` | implemented (W3); `--circle` constrains the bound email rather than creating a shared circle credential |
| `POST /api/v1/hosts/invites` (master) | `bashy paired-host invite` (pairing code for your own account) | implemented |
| `POST /api/v1/hosts/pair` | `outpost register --code CODE` — identity-only when the server is the master | implemented (outpost 44db24d) |
| `DELETE /api/v1/hosts/self` (master) | `outpost unpair` | implemented (outpost 44db24d) |
| `GET /api/v1/satellites` (owner) | `bashy satellite list` | exists |
| `GET /api/v1/satellites` (host) | `outpost satellite list` | implemented (outpost d7963fa) |
| `GET /api/v1/satellites/:id` | `bashy satellite show ID` | implemented — direct ID lookup; unique names resolve through the list on 404 |
| `GET /api/v1/satellites/assignment` | `bashy paired-host ls --satellite ID` | implemented — filters hosts and peers using the Bearer assignment list |
| `GET/POST/DELETE /api/v1/hosts/:host/shares` (satellite/standalone) | `bashy outpost apps share APP --email E` / `unshare` / `shares` (journey: `satellite-member share`) | implemented (cloudbox S7, outpost 993631d) |
| `POST /api/v1/satellites/:id/attach` + satellite `POST /api/v1/hosts/attach` | `outpost satellite attach ID` (one verb, both calls) | implemented (outpost d7963fa) |
| `DELETE /api/v1/satellites/:id/attach` + satellite `DELETE /api/v1/hosts/self` | `outpost satellite detach ID` | implemented (outpost d7963fa) |

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
  `bashy llm peer add bob --email bob@example.com --circle family` (active,
  mutual members only). The door checks the email and current circle on every
  request. If the master is unreachable it may use its last known member list
  for at most 24 hours, records that list's age in the audit line, then refuses.

## 7. Error codes

`bad_request`, `unauthorized`, `insufficient_scope`, `not_found`,
`invalid_email`, `invalid_circle`, `self_member`, `invitation_expired`,
`idempotency_mismatch`, `invalid_code`, `code_expired`, `code_consumed`,
`host_exists`, `satellite_unavailable`,
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
4. **Circles** — owner-defined names: a lowercase slug `[a-z0-9-]` of 1–32
   chars; `public` and `owner` are reserved. `family|friend|team|guest` are
   default suggestions; default for a new member is `guest`. (Supersedes the
   earlier closed-set ruling; `team:<name>` syntax is dropped in favor of
   plain slugs.)
5. **Several satellites per host** — assignments are separate host and satellite rows.

## 8. Circles and app suites (master)

This section is the S9 public wire contract shared by the cloudbox server,
this fake and its clients.

### Shared rule shape

Host and suite shares use the rule shape in §2b. A group may hold the owner
or current members; removing a member also removes their group rows. Deleting
a group removes that group from every grant that names it.

### Suites (owner-account level named set of host/app)

- `GET /api/v1/suites` -> `{"suites":[{"name","apps":[{"host","app"}],"created_at","updated_at"}]}`
- `GET /api/v1/suites/{name}` -> `{"suite":{...}}` 404 not_found
- `PUT /api/v1/suites/{name}` `{"apps":[{"host","app"}]}` create or replace; every host must be owned by the caller (else 403 not_owner)
- `POST /api/v1/suites/{name}/apps` `{"host","app"}` add (idempotent)
- `DELETE /api/v1/suites/{name}/apps/{host}/{app}` remove (idempotent)
- `DELETE /api/v1/suites/{name}` delete suite and its grants
- `GET /api/v1/suites/{name}/shares` -> `{"shares":[{"id","suite","circles","groups","emails","created"}]}`
- `POST /api/v1/suites/{name}/shares` `{"circles":[],"groups":[],"emails":[]}`
- `DELETE /api/v1/suites/{name}/shares/{id}`

Errors: `{"error": "<code>"}` with codes invalid_suite, invalid_circle,
invalid_share, not_found, not_owner.

### Access semantics

Host and suite grants use the same rule evaluation in §2b, judged against
the host owner. A suite grant applies when its suite contains the host/app;
its audience is still evaluated against the host owner. Membership, circle,
group and suite edits take effect on the next evaluation.

## 9. Satellite admission

Satellite admission is a separate list of the same rules, judged against the
satellite owner. It does not reference a host grant. The satellite owner
always passes; an empty list admits the owner only.

- `GET /api/v1/satellites/:id/admit` → `200 {"satellite":"ID","rules":[...]}`.
- `PUT /api/v1/satellites/:id/admit` body `{"rules":[...]}` → the same response.

The CLI supports `bashy satellite admit ls SAT [--json]`, `add SAT
[--circle C]... [--group G]... [--email E]...`, `rm SAT INDEX` (zero-based),
and `set SAT --json-rules FILE|-`. Mutations accept `--dry-run`.
