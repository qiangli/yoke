# Typed service API adapters — story cf3c288d595a (sprint 457, seq 2298)

Yoke-side typed adapters for catalog gaps with no usable native CLI.
Fixture/loopback tests are distinct from live support: nothing here ran
against a real account, credential, tenant or network service.

## Selection (from the service inventory)

Source: `research/n8n-cli/catalog.csv` (571 rows) and
`research/n8n-cli/candidates.csv`, both read 2026-10-18 in the umbrella
checkout. The catalog was used only to SELECT operations and to record
the remaining families below; no n8n source was vendored, copied, or
depended on.

| Service | Catalog nodes | Native CLI position | This story |
|---|---|---|---|
| Slack | `n8n-nodes-base.slack`, `n8n-nodes-base.slackTrigger` | `slack` CLI (slackapi/slack-cli, Apache-2.0) is app tooling — manifest, deploy, triggers, datastore — and does not read or post messages (candidates.csv: "API command is real; Events API/Socket Mode processing and acknowledgment require adapter") | Typed data actions (history, channel-list, message-post, message-delete) over the Web API through the merged `saasclients` adapter — reused, not duplicated (`slack.go` delegates post/transport/classification to it) |
| Notion | `n8n-nodes-base.notion`, `n8n-nodes-base.notionTrigger` | No native CLI; the curl candidate covers Notion as generic HTTP only | Typed page-get/create/update/archive + database-query (`notion.go`) |
| Airtable | `n8n-nodes-base.airtable`, `n8n-nodes-base.airtableTrigger` | No native CLI; generic HTTP only | Typed record-list/get/create/update/delete (`airtable.go`) |

## Public API contracts (implemented from, pinned)

| Provider | Contract revision | Reference |
|---|---|---|
| Notion | Integration API version `2022-06-28`, sent as the `Notion-Version` header on every request (const `NotionVersion`); the loopback fake rejects a missing/wrong pin | https://developers.notion.com/reference/intro |
| Airtable | Web API as documented 2026-10-18 (no provider version header exists; base path `/v0/{base}/{table}`) | https://airtable.com/developers/web/api/introduction |
| Slack | Web API methods `conversations.history`, `conversations.list`, `chat.postMessage`, `chat.delete` via the merged `saasclients` adapter | https://docs.slack.dev/reference/methods |

New code only; no third-party code is vendored. `go.mod`/`go.sum`
untouched (stdlib plus the existing `external/gws` and
`external/saasclients` imports). The three remote services are
proprietary hosted services under the account holder's own agreement;
client/adapter code here grants no service approval.

## Shared-facility reuse (additive changes to `external/saasclients`)

- `scope.go`: two `scopeSpecs` rows (`notion`, `airtable`) so the new
  adapters resolve explicit target + credential-file scope through the
  existing secret facility — no ambient fallback, no second facility.
- `ops.go`: five notion + five airtable `Ops` capability rows, all
  `ViaAPI`, every mutation with an idempotency strategy (creates
  `IdemLedger`, id-keyed updates/deletes `IdemNaturalID`).
- `api.go`: two provider-error shapes (`Notion
  {"object":"error",...}`, Airtable `{"error":{"type":...}}`) plus
  their documented codes mapped onto the shared `gws` codes;
  unrecognized codes still classify by HTTP status.
- Existing `saasclients` behavior is unchanged: its full suite passes
  unmodified (its `TestCapabilityScopeNotAssumed` invariant covers the
  new rows automatically).

Typed errors, cursor binding checks, redaction and the loom-v2
envelopes are `external/gws` throughout: no new error ABI, no new
envelope, no new cursor serialization (provider cursors travel verbatim
under `gws.Binding` checks; concrete serialization stays with 446 S19).

## Adapter matrix

| Op | Via | Effect | Idem | Paging | Retry | Envelope |
|---|---|---|---|---|---|---|
| slack history / channel-list | Web API GET | read | — | provider cursor, binding-checked, max 200/page | bounded (Retry-After honored) | `apiadapters-v1` typed + loom-v2 via `EmitPage` |
| slack message-post | Web API (delegates `saasclients.SlackPost`) | real-world, sandbox-only | ledger | — | never | same |
| slack message-delete | Web API | delete | natural-id (ts) | — | never | same |
| notion page-get / database-query | REST | read | — | start_cursor/has_more, binding-checked, max 100/page | bounded | same |
| notion page-create | REST | write | ledger | — | never | same |
| notion page-update / page-archive | REST | write / delete | natural-id | — | never | same |
| airtable record-list / record-get | REST | read | — | offset, binding-checked, max 100/page | bounded | same |
| airtable record-create | REST | write | ledger | — | never | same |
| airtable record-update / record-delete | REST | write / delete | natural-id | — | never | same |

Throttling: per-provider client-side minimum intervals (Slack/Notion
350ms, Airtable 250ms, following documented limits) via `Throttle`;
`nil` disables. Dry runs validate fully and send nothing. Mutations
report confirmed/failed/unknown; ledger releases failed keys for retry
and refuses replays.

## Registry / atlas

These adapters are NOT registry externals: the package ships no binary,
registers no command, and adds no atlas row. Atlas external-count delta:
**+0**. (If a future story fronts them with a CLI command, that story
owns the registry entry and states its own delta.)

## Acceptance evidence

| Item | Evidence |
|---|---|
| Typed versioned input/output | every input/output carries `schema_version: apiadapters-v1`; `TestVersionGate` rejects others with `unsupported-capability` |
| Pagination + cursors | two-page loopback walks for all three providers; cross-binding cursor reuse fails `cursor-mismatch`; oversize pages fail `output-limit` before transport |
| Throttling | `TestThrottleEnforcesInterval` (lower-bound gate); per-provider defaults in constructors |
| Retry | flaky-then-well read retried exactly twice honoring `Retry-After`; persistent 500 stops after `DefaultAttempts`; failing mutations hit the provider exactly once (`TestReadRetryAndMutationNoRetry`) |
| Mutation effects | dry-run sends nothing; ledger replays refused (`partial-unknown-mutation`); failed keys released; sandbox gate inherited from `saasclients` (non-sandbox post never reaches the provider); reversible cleanup in every lifecycle test (post→delete, create→archive/delete) |
| Mocked error paths | 401/403-shaped, 404, 429+`Retry-After`, 500, oversize body, cancelled context — all assert typed codes (`TestTypedErrorPaths`) |
| Redaction canaries | fakes echo the presented token in error bodies; message, loom-v2 envelope, provider code and argv surfaces assert no leak (`TestRedactionCanary`) |
| Remaining families recorded | package doc + below: unsupported or generic HTTP, never n8n parity |

Tests run: `go test ./external/apiadapters/ ./external/saasclients/
./external/gws/` — all ok. `go build ./cmd/yoke`, `go vet` on the
touched packages — clean. `gofmt` clean. No podman/ollama, no network,
no scratch output left behind.

## Named unverified gaps (move to Sprint 465)

- Authorized reversible account tests: Slack sandbox workspace
  (post→delete), Notion integration (page create→archive against a test
  parent), Airtable base (record create→delete); denied/expired-auth
  against the real APIs. No credentials exist here.
- Live confirmation of the op mapping (params, page maxima, cursor
  shapes) against the real APIs at the pinned contract revisions.
- Notion-Version drift (silent provider changes under the pin) and
  Airtable error-type coverage beyond the exercised shapes.
- Durable cross-process idempotency ledger (in-memory only, inherited
  from `saasclients`).
- Triggers for all three services (Slack Events/Socket Mode, Notion and
  Airtable polling) need events-story (5548854717bc) admission; refused
  here by design.

## Remaining families (unsupported or generic HTTP here)

- Notion search/users/comments/file-upload; Airtable batch writes,
  attachments, views management, webhooks: generic HTTP via curl only.
- All other catalog services keep their owning 457 story (gh, glab,
  rclone, aws, az, gcloud, m365, gam/gws, sf, stripe, twilio, database,
  messaging clients) or generic HTTP via curl where no story exists.
- Salesforce/Stripe/Twilio data operations are unchanged under the
  business story (`c512b342864d`); this story adds no rows for them.
- No claim of full n8n parity is made anywhere in this package.
