# gws evidence — story f25d7aa4437c (sprint 457, seq 2291)

Yoke-side admission and verification record for the Google Workspace CLI
integration. Source/documentation inspection, build/license admission,
fixture tests, and live verified support are distinct rows below; a
registered command, a discovered binary, or a passing mock is NOT live
support.

## Admission (reviewed)

- Candidate: gws — `googleworkspace/cli`, Apache-2.0.
- Reviewed release: v0.22.5
  (`https://github.com/googleworkspace/cli/releases/tag/v0.22.5`).
- Immutable commit: `a3768d0e82ad83cca2da97724e46bea4ff0e6dbd`.
- Service-registry source (reviewed tree's service list):
  `https://raw.githubusercontent.com/googleworkspace/cli/a3768d0e82ad83cca2da97724e46bea4ff0e6dbd/crates/google-workspace/src/services.rs`
  sha256 `846c204a4ed20841891bb26f84032cf1166f8c9c003175c84437c98758223dba`.
- Release assets (per platform): `google-workspace-cli-<arch>-<os>.tar.gz`
  (`.zip` on Windows); see `Spec`/`assetMatch` in `gws.go`. The musl Linux
  variant is deliberately unmatched (unverified alternative).
- Mode: short-lived client only (supply-chain Rule 2). No server,
  long-lived, webhook-forwarding, or resident mode is admitted.
- Notices: gws is pre-v1 (breaking changes possible between releases;
  upgrades are deliberate reviewed tag bumps); gws is NOT an officially
  supported Google product.

## Consumed shared contracts (no private ABI)

- Native result/error envelope: loom-v2 (`coreutils/pkg/weavecli`,
  `schema_version: loom-v2`). The ONE discriminator spelling everywhere is
  `schema_version`. No 457 error/cursor/envelope ABI is introduced.
- Pagination: opaque provider tokens bound to (target, query, revision);
  cross-binding reuse fails typed (`cursor-mismatch`) with an explicit
  restart/resync choice. The concrete cursor serialization is
  bashy-cursor-v1, owned by 446 S19 and not yet implemented in this repo —
  tokens here stay opaque and are never parsed by consumers.
- Service capability data (`services.go`) is 457-owned mapping consumed by
  446 adapt rows / lens parsers, not a competing registry or parser.
- Credential selection reuses explicit env + file checks; no new secret
  store (453 owns storage). Redaction validated with canary secrets
  (`errors_effect_redact_test.go`: argv, provider error bodies).

## Deferred / unclaimed (not absent)

- `gmail:watch`, `events:subscribe`, `events:renew` — owned by the
  durable-event story 5548854717bc, delivered as opt-in adapters in
  `external/gws/events` (section below); the gws service map still refuses
  them as plain gws operations (`Unsupported()` + `services_test.go`).
- `admin:*` (directory, users, groups, orgunits, mobile, chromeos) —
  UNCLAIMED until tested; owned by story 66494446f180.

## Verification status

- Fixture tests (this repo, no network, no credentials): PASS —
  `go test -short -count=1 ./external/gws/ ./external/registry/`
  (see commit message for exact exit codes). Covers pin, asset match,
  nine admitted services, deferrals, explicit credential selection and
  ambiguity refusal, redacted scope, opaque-token pagination incl.
  cross-binding/foreign-token rejection, JSON/NDJSON envelopes with the
  single `schema_version` discriminator, upload/dry-run validation,
  typed-error exits, canary-secret redaction, and Discovery inspection
  incl. drift/identity-change rejection (`discovery_test.go`).
- Runtime Discovery inspection and cache drift: covered OFFLINE —
  `InspectDiscovery` parses a Discovery document already in hand
  (name/version/revision + sha256 fingerprint of the exact bytes) and
  `CheckDrift` fails closed on revision drift or identity change instead
  of silently mixing revisions (`discovery.go`, `discovery_test.go`).
  Record the observed identity alongside `Binding.Revision` (page.go).
  Fetching a live document still needs an authorized test account and is
  owned by Sprint 465 story 534ede495eb8 (below).
- Registry gate: `TestNoDefaultRegistryEntryLacksPin` unaffected (gws is
  host-first `PreferHost`, same posture as gcloud); atlas platform
  declaration added (`pkg/atlas/platform.go`, one row) so
  `TestEveryExternalDeclaresPlatforms` stays green.
- LIVE GOOGLE CHECKS — MOVED TO SPRINT 465 (story 534ede495eb8, NOT
  attempted here): no authorized test account or platform credential
  exists in this environment, so the one read + one reversible write
  with cleanup, live Discovery-schema fetch, live cache-drift
  observation, and denied/expired-auth against the real API were NOT
  performed. No live support is claimed or inferred.
- Supply-chain gaps (named, not inferred): sha256 pins for v0.22.5 are
  not yet in `pkg/binmgr/pins.go` (needs network-verified hashes — out
  of this story's scope); until then an explicit `$GWS_VERSION` managed
  download verifies against the release sidecar only (TOFU). Live
  Discovery-schema fetch and live cache-drift observation are owned by
  Sprint 465 story 534ede495eb8 (a binary pin does not freeze the remote
  API; the offline inspection half is covered above).
- Go-side dependencies: this story adds NO new module dependencies
  (`go.mod`/`go.sum` untouched — the package reuses `binmgr`,
  `coreutils/pkg/weavecli`, `policy/audit`, `notrack` only).

## Acceptance evidence (story f25d7aa4437c)

| Acceptance item | Evidence (test / file) |
|---|---|
| gws via existing external registry | `external/registry/gws.go`, `TestGwsEntry` / `TestGwsResolvePinned` (`external/registry/gws_test.go`) |
| Reviewed release pin (Apache-2.0, v0.22.5 @ a3768d0e) + dependencies | `TestPin`, `TestSpecDefaultsToPin`, `TestAssetMatch` (`gws_test.go`); no new Go deps (go.mod untouched) |
| Runtime Discovery schema inspection and cache drift | `discovery.go` (`InspectDiscovery`, `CheckDrift`), `TestInspectDiscoveryIdentity` / `TestInspectDiscoveryRejects` / `TestCheckDrift` (`discovery_test.go`) — offline half; live fetch in Sprint 465 |
| Explicit OAuth or service-account credentials, no ambient fallback | `auth.go`, `TestResolve*` + `TestRedactedScope` (`auth_test.go`) |
| JSON/NDJSON pagination, single `schema_version` discriminator | `page.go` (`EmitPage`, `EmitNDJSON`), `TestPaginateFirstAndResume` / `TestTokenCrossBindingRejected` / `TestForeignAndRangedTokens` (`page_test.go`); opaque tokens stay binding-scoped, bashy-cursor-v1 stays 446-owned (no private cursor ABI) |
| Uploads (Drive) and dry runs (all nine services) | `effect.go` (`Upload.Validate`, `ReadBounded`, `DryRun.Check/Emit`), `TestUploadValidation` / `TestReadBounded` / `TestDryRun` (`errors_effect_redact_test.go`); per-service Ops in `services.go`, `TestAdmittedServices` (`services_test.go`) |
| 446 adapter/schema contracts (loom-v2 envelope, four-level preview) | `errors.go` (typed codes on the shared exit mapping), `Preview*` levels (`services.go`), `TestErrorExits` / `TestErrorRetryable` / `TestUnsupportedNamesHome` / `TestEmitErrorEnvelope` |
| 449 discovery (cloud tier taxonomy) | registry `Tier: 6`, atlas row `pkg/atlas/platform.go`, `TestEveryExternalDeclaresPlatforms` stays green |
| Error handling and redaction (offline half) | `redact.go`, `TestRedactArgsCanary` / `TestRedactTextCanary` |
| Pre-v1 breaking-change + no-official-Google-support notices | `gws.go` package doc + `NewGwsCmd` long help, registry `Long`, Admission section above |
| Admin Directory CRUD unclaimed until tested | `Deferred` map + `LookupService`, `TestDeferred` / `TestLookupService` |
| Gmail watch, Events subscribe/renew subject to event story | `Deferred` map + `Unsupported()`, `TestDeferred` (names story 5548854717bc) |

Moved to Sprint 465 story 534ede495eb8 (NOT attempted here): read and
reversible write with an authorized test account, cleanup, live
Discovery-schema fetch, live cache-drift observation, and
denied/expired-auth error handling against the real API.

## Durable event adapters — story 5548854717bc (sprint 457, seq 2297)

Package `external/gws/events`. Builds on this record (same candidate, same
pin); adds NO Go module dependency, NO registry entry, and NO command — the
documented external count is unchanged.

### Execution modes

| Mode | Classification | Status |
|---|---|---|
| `Poller.RunOnce` (Gmail `users.history.list` via the registered gws command) | short-lived client: one bounded walk (`MaxBatches`, `MaxHistoryPages`), returns when caught up / bounded / failed / cancelled | enabled as a library step; run it from the existing scheduler, not a resident loop |
| `Renewer.RenewDue` + `RenewalJob` (Gmail `users.watch`, Events `subscriptions.patch` ttl) | short-lived client per renewal, cadence from the existing scheduler (`pkg/schedule` kind `every`) | enabled; cadence refused if `every + margin` exceeds the lease lifetime |
| `Receiver` (Pub/Sub push handler for Gmail watch + Workspace Events) | **long-lived service** when hosted (service Rule 1) | `http.Handler` only — the package never listens (`TestNoListenerInPackage`); hosting is DISABLED by `ServeAdmission()` until a Bashy-signed build with no-egress, local/LAN-bind and outpost-TLS evidence exists |

No upstream-built service is started; nothing listens, persists or renews
unless the caller opts in (explicit checkpoint dir, explicit mount,
explicit schedule).

### Processing boundary and delivery guarantee

AT-LEAST-ONCE, never exactly-once. An event is acknowledged (recorded in the
checkpoint's bounded dedup window, fsync + atomic rename) only after its
`Handler` returned nil; the change cursor advances only after every event of
its batch is acknowledged, in provider order. A crash between the downstream
action and the acknowledgment redelivers the event on restart, so handlers
must be idempotent on `Event.ID`. Handler failures are reported as
`partial-unknown-mutation` and are never retried by the adapter (no blind
retry of ambiguous writes). Fetching a page is not acknowledgment: provider
`pageToken` pagination is walked inside one `Poll` and is never stored as the
cursor. Receiver answers 2xx only after handler + acknowledgment, so Pub/Sub
redelivers anything else.

### Reused machinery (no competing schema / ABI / store)

- Typed errors: `gws.Error` on the shared loom-v2 exit mapping; one code
  added to the existing list, `cursor-expired` (the contract's "cursor
  mismatch/expiry" distinction). No new error ABI.
- Cursors: opaque provider tokens; checkpoint bound to `gws.Binding`
  (target, query, revision) and rejected with `cursor-mismatch` elsewhere.
  bashy-cursor-v1 serialization stays 446 S19-owned.
- Claims: coreutils `pkg/lockfile` (one consumer per checkpoint).
- Scheduler: coreutils `pkg/schedule.Job` (kind `every`) over the registered
  `gws` command argv. Execution: `GwsRunner(bin)` runs only the gws binary
  resolved from the existing registry entry (`binmgr.Command`, `notrack` env,
  bounded stdout) — never an arbitrary PATH command.
- Redaction: `gws.RedactText` literal masking of declared secrets on every
  returned error (provider error bodies, subprocess failures, handler errors).
- State: `gws-events-checkpoint-v1` JSON file (0600) holding cursor, dedup
  window, leases and explicit resync records only — event-adapter state, NOT
  a general database and not a replacement for Sprint 453 storage APIs.

### Acceptance evidence (fixture tests, no network, no credentials)

| Acceptance item | Evidence (test / file) |
|---|---|
| Explicit opt-in durable checkpoint storage | `checkpoint.go` `Open` (explicit dir, 0600, atomic write, binding check, single-consumer lock); `TestOpenIsOptInAndClaimed`, `TestCheckpointHoldsNoCredentials` |
| Deduplication / idempotency | bounded `Seen` window, `Event.ID` key; `TestDuplicateDelivery`, `TestDedupWindowIsBounded`, `TestReceiverDuplicateDeliveryAndAckOrdering` (CloudEvents `ce-id` across new Pub/Sub messageIds) |
| Retry / backoff / rate limits | `retry.go` (`Backoff`, retry-after honoured and capped, only `Retryable` reads, `Limiter`); `TestRetryBackoffRateLimitAndRetryAfter`, `TestGmailErrorClassificationAndRedaction` (429 / 403 rateLimitExceeded → rate-limit) |
| Ordered acknowledgment | `poller.go` `RunOnce`; `TestOrderedAckAndRestartRecovery` |
| Cancellation | ctx checked per event and in every backoff/limiter wait; `TestCancellationKeepsCheckpoint` (cancel + deadline → `cancelled`/`timeout`, checkpoint not advanced past completed processing) |
| Restart recovery (incl. crash between action and ack) | `TestOrderedAckAndRestartRecovery` (handler fails mid-batch, store reopened, acked event skipped as duplicate, rest delivered in order) |
| Expired cursors | Gmail 404 / 410 → `cursor-expired`, not retried, checkpoint untouched, explicit `Resync(cursor, reason)` recorded; `TestExpiredCursorNeedsExplicitResync`, `TestGmailErrorClassificationAndRedaction`; lapsed lease → `cursor-expired` (`TestEventsSubscriptionLeaseRenewal`) |
| Pagination distinct from acknowledgment | `TestGmailHistoryPaginationIsNotTheCursor`, `TestGmailHistoryBoundIsResumable` |
| Receiver authentication | `SharedToken` (constant-time, `?token=` / `X-Goog-Channel-Token`), `BearerVerifier` (caller-supplied OIDC verifier); `TestReceiverAuthentication`, `TestReceiverBearerVerifier`, `TestReceiverBoundsAndMalformedInput` |
| Gmail watch lease renewal (fake) | `CommandRenewer` + `ParseGmailWatch`; `TestGmailWatchLeaseRenewal` (retryable 503 then renewed, expiry persisted, out-of-margin lease untouched) |
| Events subscription lease renewal (fake) | `EventsRenewArgv` + `ParseEventsRenew`; `TestEventsSubscriptionLeaseRenewal` (unconfirmed LRO → unknown outcome, expiry unchanged; confirmed → persisted; non-extending → failure) |
| Existing scheduler reuse | `RenewalJob` + `schedule.ComputeNext`; `TestRenewalJobUsesExistingScheduler` |
| Hosted receiver / long-lived worker under Rule 1 | `ServeAdmission()` disabled with reason; `TestNoListenerInPackage` |
| Redaction with canary secrets | `TestGmailErrorClassificationAndRedaction` (provider error body, subprocess failure, handler error), `TestReceiverAuthentication` / `TestReceiverBearerVerifier` (responses never echo tokens), `TestCheckpointHoldsNoCredentials` |
| No exactly-once promise | package doc (`events.go`) + this section |

Gate run: `go test -short -count=1 ./external/gws/... ./external/registry/`,
`go vet` of the same, `go build ./external/... ./pkg/atlas/... ./cmd/yoke`
(results in the commit message).

### Named gaps (NOT attempted; live checks move to Sprint 465)

- Live Gmail `users.watch` / `users.history.list`, live Workspace Events
  subscription create/patch, a real Pub/Sub push delivery, and a real
  expired historyId / lapsed subscription — need an authorized test account,
  GCP project and Pub/Sub topic; none exists here. Fakes only.
- gws argv mapping (`gws gmail users watch|history list`, `gws events
  subscriptions patch`, `--params` / `--json`) and the provider error-body
  shape are from source/documentation inspection, not observed against the
  pinned v0.22.5 binary; unverified until Sprint 465.
- Pub/Sub OIDC JWT verification is caller-supplied (`BearerVerifier`); this
  package ships no JWKS fetch/caching — a live verifier is unbuilt.
- Hosting the receiver: no Bashy-signed build, no-egress proof, local/LAN
  bind or outpost-TLS evidence — disabled (`ServeAdmission`).
- No front-door verb invokes `RunOnce` / `RenewDue`; a scheduler job today
  can run the renewal gws argv (`RenewalJob`), and a poll-once verb would be
  a new registered command (manager decision; would change the documented
  count).
- Dedup is bounded (`DefaultSeenWindow` = 1024 ids); older duplicates are
  not detected, which is why handlers must stay idempotent.
