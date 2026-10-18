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

- `gmail:watch`, `events:subscribe`, `events:renew` — deferred to the
  durable-event story 5548854717bc (lease renewal, checkpoint, dedup,
  restart recovery). Enforced by `Unsupported()` + `services_test.go`.
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
