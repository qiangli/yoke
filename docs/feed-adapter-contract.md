# Feed Channel Adapter Contract & Bridge Routes

Version: `bashy-feed-envelope-v1` (see `pkg/feed/envelope.go:ContractVersion`)

## Envelope

One JSON shape travels both directions. Core → adapter (outbound) and
adapter → core (inbound) use the same fields so a bridge can re-send
without translation.

```json
{
  "version": "bashy-feed-envelope-v1",
  "id": "0193e...",
  "channel": "telegram",
  "message_id": "12345",
  "conversation_id": "chat-123",
  "sender_id": "user-789",
  "sender_display": "Alice",
  "text": "hello",
  "attachments": [{"path":"/tmp/foo.png","sha256":"...","mime":"image/png","name":"foo.png","size":1234}],
  "reply_to": "12344",
  "received_at": "2026-10-13T19:00:00Z",
  "origin_chain": ["telegram:chat-123"],
  "is_dm": false
}
```

Required: `version`, `channel`, `message_id`, `conversation_id`, `sender_id`, `received_at`.
`attachments` entries require `path` and `sha256` (64 hex chars); `mime`/`name`/`size` are recommended.
`origin_chain` grows on each bridge hop for loop prevention.

## Outbound (core → adapter)

Register a channel as a bashy tool command:

```
bashy feed channel add NAME --cmd TOOL:COMMAND --args 'template args'
```

At delivery the core encodes `DeliveryRequest{version,envelopes:[]Envelope}` as
JSON and invokes the tool command with that JSON as `--message`. The adapter
reads `DeliveryRequest` (or a single `Envelope` for backwards compat) and
returns `DeliveryResult{delivered_ids:[]string}` as JSON on stdout. A missing
or empty result synthesises ids from the envelopes; any non-empty `error`
field is a failure. Retries use exponential backoff (3 attempts). No secrets
on argv — adapters read provider tokens from env vars populated by
`bashy secret env` (e.g. `TELEGRAM_BOT_TOKEN`, `MATRIX_ACCESS_TOKEN`,
`DISCORD_BOT_TOKEN`, `SLACK_BOT_TOKEN`).

Example adapter `main` (stdin/stdout):

```go
var req feed.DeliveryRequest
json.NewDecoder(os.Stdin).Decode(&req)
ids, _ := myAdapter.Send(ctx, req.Envelopes)
json.NewEncoder(os.Stdout).Encode(feed.DeliveryResult{DeliveredIDs: ids})
```

Delivery also honours quiet hours, per-channel rate, and the shared read
cursor (`feed-cursor.json`), so adding a messaging tool never needs a bashy
release — channels are adapters wired only through this contract.

## Inbound (adapter → core)

Two ingresses, same validation:

1. **NDJSON on stdout** of a supervised bashy service unit. Each line is one
   `Envelope` JSON. Core runs `ProcessNDJSON` to validate, dedupe
   (`channel:message_id`), check pairing, and fan out.
2. **Loopback POST** to `127.0.0.1` — `POST /inbound` with one `Envelope` body.
   Handled by `InboundServer.ServeHTTP`.

Policies applied on every inbound envelope:

- **Pairing allowlist** — only senders in the allowlist are admitted
  (`pairing allowlist` / `DenyAllPairing` in tests). Unpaired senders get
  `403` (HTTP) or are dropped (NDJSON).
- **Quiet hours** — `QuietHours(now,start,end)` suppresses delivery inside
  the window unless the sender is immediate.
- **Shared read cursor** — `FileCursor` (`feed-cursor.json`) / `MemCursor`.
- **Draft / approve** — `DraftStore` holds bridged drafts until
  `/approve` publishes.
- **Dedupe** — `seen` map on `OriginKey()`.

## Bridge

```
bashy feed bridge add SRC_CHANNEL:CONVERSATION DST_CHANNEL:CONVERSATION [--two-way] [--allow-dm] [--rate-limit N]
bashy feed bridge list
bashy feed bridge rm ID
```

A route maps `SRC_CHANNEL:CONVERSATION` → `DST_CHANNEL:CONVERSATION`.
`--two-way` adds the reverse edge; `--allow-dm` lets DMs cross (default: DMs
never bridged); `--rate-limit N` caps forwards per minute per route.

Forwarding:

- Inbound envelopes matching `SrcChannel+SrcConversation` are re-sent through
  the destination adapter with attribution: `"[Display via origin] text"`.
- `OriginChain` gains `channel:conversation` on each hop; any route whose
  `Dst` already appears in the chain is skipped (loop prevention).
- `message_id` dedupe prevents echo.
- Only paired senders are bridged.
- Attachments and `reply_to` threading are passed through where supported.

Example: Discord as daily UI bridging to Matrix and Telegram, Slack outbound:

```
bashy feed bridge add discord:123 matrix:!room:example.com --two-way
bashy feed bridge add discord:123 telegram:chat-456 --two-way
bashy feed bridge add telegram:chat-456 matrix:!room:example.com
```

## Reference Adapters

In `pkg/feed/adapters/` (OpenClaw MIT, tag `v2026.10.2`, converted from
TypeScript to Go):

- `telegram.go` — inbound (poll `getUpdates`) + outbound (`sendMessage`/`sendDocument`)
- `matrix.go` — inbound (`/sync`) + outbound (`PUT /rooms/.../send`)
- `discord.go` — outbound (`POST /channels/{id}/messages`) — inbound next stage
- `slack.go` — outbound (`chat.postMessage`) — inbound next stage

All adapters are wired only through the envelope contract; they hold no other
core coupling. Tests use `httptest` fakes per adapter; a fake adapter backs
the conformance suite.

## Conformance

`go test ./pkg/feed/...` runs:

- `TestAdapterConformance` — validates send/validate, JSON on stdin, delivered
  ids, NDJSON dedupe, loopback, pairing, attachments, secret isolation.
- `TestBridge*` — two-way route, loop prevention, dedupe, attribution,
  attachment passthrough, unpaired refused, DM not bridged / allowed, rate
  limits, reply threading.
- Per-adapter httptest suites for Telegram, Matrix, Discord, Slack.
