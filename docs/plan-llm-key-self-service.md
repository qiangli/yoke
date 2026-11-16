# Self-service LLM keys for circle members (`bashy llm key`)

**Epic:** `lern-psic` · **Sprint:** 648 (MVP) · **Home:** yoke `pkg/broker`
(`selfservice.go`, `selfservicecmd.go`, `keycmd.go`), surfaced as `bashy llm …`.

## What it does

A door owner lets the members of chosen circles mint their own door key. A
member who is signed in to Cloudbox and is an active mutual member in an
allowed circle runs one command and gets a key bound to their email. The
owner takes no further step, and nothing is delivered out of band.

## Owner (door host)

```sh
bashy llm up                                    # the model door is running
bashy llm self-service on --circle team2027     # opt in (off by default)
outpost-profile up SATELLITE --app llm          # only for a satellite served by an extra outpost instance
bashy llm self-service status                   # on/off, circles, self-service keys
bashy llm self-service off                      # stop new mints; existing keys keep working
```

`self-service on` turns on the signed cloud identity (`trust_cloud_identity`)
of this host's outpost app `llm`. It stores that app's SSO secret owner-only
beside the peers file (`$BASHY_HOME/cligw/self-service.json`). The app stays
public, so inference keeps using door keys exactly as before.

A satellite served by an extra outpost instance copies the `llm` entry,
including its secret, when `outpost-profile up SATELLITE --app llm` runs.
Re-run that command after `on`.

The usual peer tools apply to every self-service key, `peer:self-<email>`:

- `bashy llm peer limit self-<email> …` caps it.
- `bashy llm peer share self-<email> deny` suspends it. A later rotation keeps
  the deny.
- `bashy llm usage --peer self-<email>` reports it.

## Member

```sh
export BASHY_FLEET_TOKEN=…                     # Cloudbox account token with satellites:delegate
bashy llm key https://SATELLITE/matrix/h/HOST/app/llm
eval "$(bashy llm env --from HOST --via cloudbox)"
```

Before the first run, sign in to the satellite once in a browser. The
satellite only exchanges tokens for accounts it has admitted.

`llm key` stores the key owner-only as the door grant for `HOST` and never
prints it. Rerunning rotates the key, and the previous one returns 401.

Refusals are one line each:

| Refusal | What to do |
|---|---|
| not signed in | Set `BASHY_FLEET_TOKEN`, or sign in to the satellite. |
| not a member | Ask the owner for an invitation. |
| invitation pending | Accept the invitation in Cloudbox. |
| circle not allowed | You are in a circle the owner has not allowed. |
| self-service off | The owner has not turned self-service on. |
| door unreachable or too old | Check the URL, or ask the owner to update the door. |

## How it works

1. The CLI exchanges the member's account token at the satellite
   (`POST /api/v1/tokens/exchange`, scope `host:proxy`, 10 min). It then
   sends `POST …/app/llm/v1/peer/self` with that short-lived token as the
   Bearer. Redirects are never followed.
2. The satellite stamps the caller's email. The outpost removes any
   client-sent identity headers and signs the email with the app's SSO secret.
3. The door verifies the signature and its freshness (60 s), and refuses an
   empty identity. It then reads the member list. A mint fails closed while
   the account master is unreachable.
4. The door mints or rotates the peer `self-<email>`, bound to that email and
   the member's circle. A new peer starts at `share allow`. A rotation keeps
   the owner's share policy and limits. Writes to the peers file are locked,
   so concurrent mints and owner edits never lose records.
5. Every inference request re-checks membership through the existing member
   client: a fresh list within 30 s, or the last known list for up to 24 h
   while the master is unreachable. Removing the member from the circle
   disables the key.

**Revoke** (`llm peer revoke`) is for a leaked key. The member can mint a new
key while they are still in an allowed circle. To cut someone off, remove them
from the circle.

## Not in this MVP

- Vault storage of the member key.
- Default caps set by the self-service policy.
- A `--print` mode.
- Self-service keys over the mesh route.
- Narrowing the allowed circles does not touch keys already issued. Each key
  stays bound to the circle it was minted in. Use `peer revoke` or remove the
  member from that circle.
